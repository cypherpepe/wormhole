// The accounting package manages the interface to the accounting smart contract on wormchain. It is passed all VAAs before
// they are signed and published. It determines if the VAA is for a token bridge transfer, and if it is, it submits an observation
// request to the accounting contract. When that happens, the VAA is queued up until the accounting contract responds indicating
// that the VAA has been approved. If the VAA is approved, this module will forward the VAA back to the processor loop to be signed
// and published.

package accounting

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"sync"
	"time"

	"github.com/certusone/wormhole/node/pkg/common"
	"github.com/certusone/wormhole/node/pkg/db"
	"github.com/certusone/wormhole/node/pkg/supervisor"
	"github.com/certusone/wormhole/node/pkg/wormconn"
	"github.com/wormhole-foundation/wormhole/sdk"
	"github.com/wormhole-foundation/wormhole/sdk/vaa"

	ethCommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"go.uber.org/zap"
)

const (
	MainNetMode = 1
	TestNetMode = 2
	DevNetMode  = 3
	GoTestMode  = 4

	// We will retry requests once per minute for up to an hour.
	auditInterval = time.Duration(time.Minute)
	maxRetries    = 60
)

type (
	// tokenBridgeKey is the key to the map of token bridges being monitored
	tokenBridgeKey struct {
		emitterChainId vaa.ChainID
		emitterAddr    vaa.Address
	}

	// tokenBridgeEntry is the payload of the map of the token bridges being monitored
	tokenBridgeEntry struct {
	}

	// pendingEntry is the payload for each pending transfer
	pendingEntry struct {
		msg        *common.MessagePublication
		msgId      string
		digest     string
		updTime    time.Time
		retryCount int
	}
)

// Accounting is the object that manages the interface to the wormchain accounting smart contract.
type Accounting struct {
	ctx                  context.Context
	logger               *zap.Logger
	db                   db.AccountingDB
	contract             string
	wsUrl                string
	wormchainConn        *wormconn.ClientConn
	enforceFlag          bool
	gk                   *ecdsa.PrivateKey
	gst                  *common.GuardianSetState
	guardianAddr         ethCommon.Address
	msgChan              chan<- *common.MessagePublication
	tokenBridges         map[tokenBridgeKey]*tokenBridgeEntry
	pendingTransfersLock sync.Mutex
	pendingTransfers     map[string]*pendingEntry // Key is the message ID (emitterChain/emitterAddr/seqNo)
	subChan              chan *common.MessagePublication
	env                  int
}

const subChanSize = 50

// NewAccounting creates a new instance of the Accounting object.
func NewAccounting(
	ctx context.Context,
	logger *zap.Logger,
	db db.AccountingDB,
	contract string, // the address of the smart contract on wormchain
	wsUrl string, // the URL of the wormchain websocket interface
	wormchainConn *wormconn.ClientConn, // used for communicating with the smart contract
	enforceFlag bool, // whether or not accounting should be enforced
	gk *ecdsa.PrivateKey, // the guardian key used for signing observation requests
	gst *common.GuardianSetState, // used to get the current guardian set index when sending observation requests
	msgChan chan<- *common.MessagePublication, // the channel where transfers received by the accounting runnable should be published
	env int, // Controls the set of token bridges to be monitored
) *Accounting {
	return &Accounting{
		ctx:              ctx,
		logger:           logger,
		db:               db,
		contract:         contract,
		wsUrl:            wsUrl,
		wormchainConn:    wormchainConn,
		enforceFlag:      enforceFlag,
		gk:               gk,
		gst:              gst,
		guardianAddr:     ethCrypto.PubkeyToAddress(gk.PublicKey),
		msgChan:          msgChan,
		tokenBridges:     make(map[tokenBridgeKey]*tokenBridgeEntry),
		pendingTransfers: make(map[string]*pendingEntry),
		subChan:          make(chan *common.MessagePublication, subChanSize),
		env:              env,
	}
}

// Run initializes the accounting module and starts the watcher runnable.
func (acct *Accounting) Start(ctx context.Context) error {
	acct.logger.Debug("acct: entering run")
	acct.pendingTransfersLock.Lock()
	defer acct.pendingTransfersLock.Unlock()

	emitterMap := sdk.KnownTokenbridgeEmitters
	if acct.env == TestNetMode {
		emitterMap = sdk.KnownTestnetTokenbridgeEmitters
	} else if acct.env == DevNetMode || acct.env == GoTestMode {
		emitterMap = sdk.KnownDevnetTokenbridgeEmitters
	}

	// Build the map of token bridges to be monitored.
	for chainId, emitterAddrBytes := range emitterMap {
		emitterAddr, err := vaa.BytesToAddress(emitterAddrBytes)
		if err != nil {
			return fmt.Errorf("failed to convert emitter address for chain: %v", chainId)
		}

		tbk := tokenBridgeKey{emitterChainId: chainId, emitterAddr: emitterAddr}
		_, exists := acct.tokenBridges[tbk]
		if exists {
			return fmt.Errorf("detected duplicate token bridge for chain: %v", chainId)
		}

		tbe := &tokenBridgeEntry{}
		acct.tokenBridges[tbk] = tbe
		acct.logger.Info("acct: will monitor token bridge:", zap.Stringer("emitterChainId", tbk.emitterChainId), zap.Stringer("emitterAddr", tbk.emitterAddr))
	}

	// Load any existing pending transfers from the db.
	if err := acct.loadPendingTransfers(); err != nil {
		return fmt.Errorf("failed to load pending transfers from the db: %w", err)
	}

	// Start the watcher to listen to transfer events from the smart contract.
	if acct.env != GoTestMode {
		if err := supervisor.Run(ctx, "acctworker", acct.worker); err != nil {
			return fmt.Errorf("failed to start submit observation worker: %w", err)
		}

		if err := supervisor.Run(ctx, "acctwatcher", acct.watcher); err != nil {
			return fmt.Errorf("failed to start watcher: %w", err)
		}
	}

	return nil
}

func (acct *Accounting) Close() {
	if acct.wormchainConn != nil {
		acct.wormchainConn.Close()
		acct.wormchainConn = nil
	}
}

func (acct *Accounting) FeatureString() string {
	if !acct.enforceFlag {
		return "acct:logonly"
	}
	return "acct:enforced"
}

// SubmitObservation will submit token bridge transfers to the accounting smart contract. This is called from the processor
// loop when a local observation is received from a watcher. It returns true if the observation can be published immediately,
// false if not (because it has been submitted to accounting).
func (acct *Accounting) SubmitObservation(msg *common.MessagePublication) (bool, error) {
	msgId := msg.MessageIDString()
	acct.logger.Debug("acct: in SubmitObservation", zap.String("msgID", msgId))
	// We only care about token bridges.
	tbk := tokenBridgeKey{emitterChainId: msg.EmitterChain, emitterAddr: msg.EmitterAddress}
	if _, exists := acct.tokenBridges[tbk]; !exists {
		if msg.EmitterChain != vaa.ChainIDPythNet {
			acct.logger.Debug("acct: ignoring vaa because it is not a token bridge", zap.String("msgID", msgId))
		}

		return true, nil
	}

	// We only care about transfers.
	if !vaa.IsTransfer(msg.Payload) {
		acct.logger.Info("acct: ignoring vaa because it is not a transfer", zap.String("msgID", msgId))
		return true, nil
	}

	digest := msg.CreateDigest()

	acct.pendingTransfersLock.Lock()
	defer acct.pendingTransfersLock.Unlock()

	// If this is already pending, don't send it again.
	if oldEntry, exists := acct.pendingTransfers[msgId]; exists {
		if oldEntry.digest != digest {
			digestMismatches.Inc()
			acct.logger.Error("acct: digest in pending transfer has changed, dropping it",
				zap.String("msgID", msgId),
				zap.String("oldDigest", oldEntry.digest),
				zap.String("newDigest", digest),
			)
		} else {
			acct.logger.Info("acct: blocking transfer because it is already outstanding", zap.String("msgID", msgId))
		}
		return false, nil
	}

	// Add it to the pending map and the database.
	if err := acct.addPendingTransfer(msgId, msg, digest); err != nil {
		acct.logger.Error("acct: failed to persist pending transfer, blocking publishing", zap.String("msgID", msgId), zap.Error(err))
		return false, err
	}

	// This transaction may take a while. Pass it off to the worker so we don't block the processor.
	if acct.env != GoTestMode {
		acct.logger.Info("acct: submitting transfer to accounting for approval", zap.String("msgID", msgId), zap.Bool("canPublish", !acct.enforceFlag))
		acct.submitObservation(msg)
	}

	// If we are not enforcing accounting, the event can be published. Otherwise we have to wait to hear back from the contract.
	return !acct.enforceFlag, nil
}

// AuditPending audits the set of pending transfers for any that have been in the pending state too long. This is called from the processor loop
// each timer interval. Any transfers that have been in the pending state too long will be resubmitted. Any that has been retried too many times
// will be logged and dropped.
func (acct *Accounting) AuditPendingTransfers() {
	acct.logger.Debug("acct: in AuditPendingTransfers")
	acct.pendingTransfersLock.Lock()
	defer acct.pendingTransfersLock.Unlock()

	if len(acct.pendingTransfers) == 0 {
		acct.logger.Debug("acct: leaving AuditPendingTransfers, no pending transfers")
		return
	}

	for msgId, pe := range acct.pendingTransfers {
		acct.logger.Debug("acct: evaluating pending transfer", zap.String("msgID", msgId), zap.Stringer("updTime", pe.updTime))
		if time.Since(pe.updTime) > auditInterval {
			pe.retryCount += 1
			if pe.retryCount > maxRetries {
				acct.logger.Error("acct: stuck pending transfer has reached the retry limit, dropping it", zap.String("msgId", msgId))
				acct.deletePendingTransfer(msgId)
				continue
			}

			acct.logger.Error("acct: resubmitting pending transfer",
				zap.String("msgId", msgId),
				zap.Stringer("lastUpdateTime", pe.updTime),
				zap.Int("retryCount", pe.retryCount),
			)

			pe.updTime = time.Now()
			acct.submitObservation(pe.msg)
		}
	}

	acct.logger.Debug("acct: leaving AuditPendingTransfers")
}

// publishTransfer publishes a pending transfer to the accounting channel and updates the timestamp. It assumes the caller holds the lock.
func (acct *Accounting) publishTransfer(pe *pendingEntry) {
	if acct.enforceFlag {
		acct.logger.Debug("acct: publishTransfer: notifying the processor", zap.String("msgId", pe.msgId))
		acct.msgChan <- pe.msg
	}

	acct.deletePendingTransfer(pe.msgId)
}

// addPendingTransfer adds a pending transfer to both the map and the database. It assumes the caller holds the lock.
func (acct *Accounting) addPendingTransfer(msgId string, msg *common.MessagePublication, digest string) error {
	acct.logger.Debug("acct: addPendingTransfer", zap.String("msgId", msgId))
	if err := acct.db.AcctStorePendingTransfer(msg); err != nil {
		return err
	}

	pe := &pendingEntry{msg: msg, msgId: msgId, digest: digest, updTime: time.Now()}
	acct.pendingTransfers[msgId] = pe
	transfersOutstanding.Inc()
	return nil
}

// deletePendingTransfer deletes the transfer from both the map and the database. It assumes the caller holds the lock.
func (acct *Accounting) deletePendingTransfer(msgId string) {
	acct.logger.Debug("acct: deletePendingTransfer", zap.String("msgId", msgId))
	if _, exists := acct.pendingTransfers[msgId]; exists {
		transfersOutstanding.Dec()
		delete(acct.pendingTransfers, msgId)
	}
	if err := acct.db.AcctDeletePendingTransfer(msgId); err != nil {
		acct.logger.Error("acct: failed to delete pending transfer from the db", zap.String("msgId", msgId), zap.Error(err))
		// Ignore this error and keep going.
	}
}

// loadPendingTransfers loads any pending transfers that are present in the database. This method assumes the caller holds the lock.
func (acct *Accounting) loadPendingTransfers() error {
	pendingTransfers, err := acct.db.AcctGetData(acct.logger)
	if err != nil {
		return err
	}

	for _, msg := range pendingTransfers {
		msgId := msg.MessageIDString()
		acct.logger.Info("acct: reloaded pending transfer", zap.String("msgID", msgId))

		digest := msg.CreateDigest()
		pe := &pendingEntry{msg: msg, msgId: msgId, digest: digest} // Leave the updTime unset so we will query this on the first audit interval.
		acct.pendingTransfers[msgId] = pe
		transfersOutstanding.Inc()
	}

	if len(acct.pendingTransfers) != 0 {
		acct.logger.Info("acct: reloaded pending transfers", zap.Int("total", len(acct.pendingTransfers)))
	} else {
		acct.logger.Info("acct: no pending transfers to be reloaded")
	}

	return nil
}

// submitObservation sends an observation request to the worker so it can be submited to the contract.
// If writing to the channel would block, this function resets the timestamp on the entry so it will be
// retried next audit interval. This method assumes the caller holds the lock.
func (acct *Accounting) submitObservation(msg *common.MessagePublication) {
	select {
	case acct.subChan <- msg:
		acct.logger.Debug("acct: submitted observation to channel", zap.String("msgId", msg.MessageIDString()))
	default:
		msgId := msg.MessageIDString()
		acct.logger.Error("acct: unable to submit observation because the channel is full, will try next interval", zap.String("msgId", msgId))
		pe, exists := acct.pendingTransfers[msgId]
		if exists {
			pe.updTime = time.Time{}
		} else {
			acct.logger.Error("acct: failed to look up pending transfer", zap.String("msgId", msgId))
		}
	}
}