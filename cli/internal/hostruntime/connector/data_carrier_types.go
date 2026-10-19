package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

// Data-carrier limits are deliberately small.  They bound both the number of
// streams which can hold a connection permit and the number of stream opens
// waiting for a peer acknowledgement.
const (
	defaultDataCarrierStreams      = 64
	defaultDataCarrierBacklog      = 16
	defaultDataCarrierQueue        = 16
	defaultDataCarrierWindow       = 256 << 10
	defaultDataCarrierKeepalive    = 30 * time.Second
	defaultDataCarrierWriteLimit   = 10 * time.Second
	defaultDataCarrierOpenLimit    = 10 * time.Second
	defaultDataCarrierCloseLimit   = 5 * time.Second
	defaultDataCarrierMaxCarriers  = 2
	maxDataCarrierStreams          = 1024
	maxDataCarrierBacklog          = 1024
	maxDataCarrierQueue            = 1024
	maxDataCarrierWindow           = 16 << 20
	maxDataCarrierKeepalive        = time.Hour
	maxDataCarrierOperationTimeout = time.Minute
)

var (
	ErrInvalidDataCarrierConfig = errors.New("invalid data carrier configuration")
	ErrDataCarrierClosed        = errors.New("data carrier closed")
	ErrDataCarrierDraining      = errors.New("data carrier draining")
	ErrDataCarrierLimit         = errors.New("data carrier stream limit reached")
	ErrDataCarrierUnavailable   = errors.New("data carrier unavailable")
	ErrDataCarrierControlOpen   = errors.New("data carrier control stream already open")
	ErrDataCarrierAdmission     = errors.New("data carrier stream admission rejected")
)

type dataCarrierAdmissionFailure struct{ cause error }

func (*dataCarrierAdmissionFailure) Error() string { return "connector stream admission failed" }

func (e *dataCarrierAdmissionFailure) Unwrap() []error {
	return []error{ErrDataCarrierAdmission, e.cause}
}

func (*dataCarrierAdmissionFailure) DiagnosticStage() string { return "peer_authority" }

func (*dataCarrierAdmissionFailure) DiagnosticCode() string { return "peer_authority_failed" }

type dataCarrierStreamOpenFailure struct {
	cause    error
	sentinel error
}

func (*dataCarrierStreamOpenFailure) Error() string { return "connector stream open failed" }

func (e *dataCarrierStreamOpenFailure) Unwrap() []error {
	if e.sentinel != nil {
		return []error{e.sentinel, e.cause}
	}
	return []error{e.cause}
}

func (*dataCarrierStreamOpenFailure) DiagnosticStage() string { return "stream_open" }

func (*dataCarrierStreamOpenFailure) DiagnosticCode() string { return "native_private_failed" }

func wrapDataCarrierStreamOpenFailure(err, sentinel error) error {
	if err == nil || connectorErrorLeaves(err, func(leaf error) bool {
		return leaf == context.Canceled || leaf == ErrDataCarrierClosed || leaf == net.ErrClosed
	}) {
		return err
	}
	return &dataCarrierStreamOpenFailure{cause: err, sentinel: sentinel}
}

// StreamOpen is the canonical connector-v1 per-stream admission metadata.
// It deliberately contains no bearer or reusable credential bytes.
type StreamOpen = connectorprotocol.StreamOpen

// DataCarrierIdentity is the authenticated control-session identity bound to
// every data-stream preface.  The carrier owner must obtain it from the
// connector-v1 control session; this package never authenticates bearer data.
type DataCarrierIdentity struct {
	AccountID   string
	HostID      string
	TunnelID    string
	ConnectorID string
	SessionID   string
	// SessionGeneration is the server-issued connector lease/credential
	// generation. It is distinct from process and configuration generations and
	// is required by stable-host update fencing.
	SessionGeneration uint64
	ProcessGeneration uint64
	Generation        uint64
}

func (i DataCarrierIdentity) validate() error {
	if connectorprotocol.ValidateIdentifier(i.AccountID) != nil || connectorprotocol.ValidateIdentifier(i.HostID) != nil || connectorprotocol.ValidateIdentifier(i.TunnelID) != nil || connectorprotocol.ValidateIdentifier(i.ConnectorID) != nil || connectorprotocol.ValidateIdentifier(i.SessionID) != nil || i.ProcessGeneration == 0 || i.Generation == 0 {
		return ErrInvalidDataCarrierConfig
	}
	return nil
}

func (i DataCarrierIdentity) matches(open StreamOpen) bool {
	return open.AccountID == i.AccountID && open.TunnelID == i.TunnelID && open.ConnectorID == i.ConnectorID && open.SessionID == i.SessionID && open.ProcessGeneration == i.ProcessGeneration && open.Generation == i.Generation
}

// DataCarrierAdmission is evaluated after the canonical stream-open preface
// has been decoded and exact session identity has matched.  The route owner
// remains responsible for route revision and request admission.
type DataCarrierAdmission struct {
	Identity  DataCarrierIdentity
	Authorize func(context.Context, StreamOpen) error
}

func (a DataCarrierAdmission) validate() error {
	if err := a.Identity.validate(); err != nil || a.Authorize == nil {
		return ErrInvalidDataCarrierConfig
	}
	return nil
}

// DataCarrierConfig controls one authenticated multiplexed carrier.  The
// carrier does not perform authentication or admission itself; callers must
// establish those boundaries before passing the link to this package.
type DataCarrierConfig struct {
	MaximumStreams       int
	AcceptBacklog        int
	QueueDepth           int
	StreamWindow         uint32
	KeepAliveInterval    time.Duration
	ConnectionWriteLimit time.Duration
	StreamOpenLimit      time.Duration
	StreamCloseLimit     time.Duration
}

func DefaultDataCarrierConfig() DataCarrierConfig {
	return DataCarrierConfig{
		MaximumStreams:       defaultDataCarrierStreams,
		AcceptBacklog:        defaultDataCarrierBacklog,
		QueueDepth:           defaultDataCarrierQueue,
		StreamWindow:         defaultDataCarrierWindow,
		KeepAliveInterval:    defaultDataCarrierKeepalive,
		ConnectionWriteLimit: defaultDataCarrierWriteLimit,
		StreamOpenLimit:      defaultDataCarrierOpenLimit,
		StreamCloseLimit:     defaultDataCarrierCloseLimit,
	}
}

func (c DataCarrierConfig) withDefaults() DataCarrierConfig {
	d := DefaultDataCarrierConfig()
	if c.MaximumStreams == 0 {
		c.MaximumStreams = d.MaximumStreams
	}
	if c.AcceptBacklog == 0 {
		c.AcceptBacklog = d.AcceptBacklog
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = d.QueueDepth
	}
	if c.StreamWindow == 0 {
		c.StreamWindow = d.StreamWindow
	}
	if c.KeepAliveInterval == 0 {
		c.KeepAliveInterval = d.KeepAliveInterval
	}
	if c.ConnectionWriteLimit == 0 {
		c.ConnectionWriteLimit = d.ConnectionWriteLimit
	}
	if c.StreamOpenLimit == 0 {
		c.StreamOpenLimit = d.StreamOpenLimit
	}
	if c.StreamCloseLimit == 0 {
		c.StreamCloseLimit = d.StreamCloseLimit
	}
	return c
}

func (c DataCarrierConfig) Validate() error {
	c = c.withDefaults()
	if c.MaximumStreams <= 0 || c.MaximumStreams > maxDataCarrierStreams {
		return fmt.Errorf("%w: maximum streams must be in [1,%d]", ErrInvalidDataCarrierConfig, maxDataCarrierStreams)
	}
	if c.AcceptBacklog <= 0 || c.AcceptBacklog > maxDataCarrierBacklog || c.AcceptBacklog > c.MaximumStreams {
		return fmt.Errorf("%w: accept backlog must be in [1,%d] and no greater than maximum streams", ErrInvalidDataCarrierConfig, maxDataCarrierBacklog)
	}
	if c.QueueDepth <= 0 || c.QueueDepth > maxDataCarrierQueue {
		return fmt.Errorf("%w: queue depth must be in [1,%d]", ErrInvalidDataCarrierConfig, maxDataCarrierQueue)
	}
	if c.StreamWindow < defaultDataCarrierWindow || c.StreamWindow > maxDataCarrierWindow {
		return fmt.Errorf("%w: stream window must be in [%d,%d]", ErrInvalidDataCarrierConfig, defaultDataCarrierWindow, maxDataCarrierWindow)
	}
	if c.KeepAliveInterval <= 0 || c.KeepAliveInterval > maxDataCarrierKeepalive {
		return fmt.Errorf("%w: keepalive interval must be in (0,%s]", ErrInvalidDataCarrierConfig, maxDataCarrierKeepalive)
	}
	if !validDataCarrierTimeout(c.ConnectionWriteLimit) || !validDataCarrierTimeout(c.StreamOpenLimit) || !validDataCarrierTimeout(c.StreamCloseLimit) {
		return fmt.Errorf("%w: operation limits must be in (0,%s]", ErrInvalidDataCarrierConfig, maxDataCarrierOperationTimeout)
	}
	return nil
}

func validDataCarrierTimeout(timeout time.Duration) bool {
	return timeout > 0 && timeout <= maxDataCarrierOperationTimeout
}

// DataCarrierState describes the lifecycle of one carrier.  A carrier is
// usable as soon as its yamux session has been built.  Client pools additionally
// perform a Ping round trip before publishing Ready.
type DataCarrierState string

const (
	DataCarrierReady   DataCarrierState = "ready"
	DataCarrierClosed  DataCarrierState = "closed"
	DataCarrierUnknown DataCarrierState = "unknown"
)

// DataCarrierStreamLink is the bounded byte stream exposed by one carrier
// stream. TCP links use yamux streams; QUIC links use native bidirectional
// streams directly.
type DataCarrierStreamLink interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// DataCarrierSession is the transport-specific multiplexing boundary. The
// TCP implementation wraps one TLS connection in yamux, while QUIC maps each
// operation to an independent native QUIC bidirectional stream.
type DataCarrierSession interface {
	OpenStream(context.Context) (DataCarrierStreamLink, error)
	AcceptStream(context.Context) (DataCarrierStreamLink, error)
	Ping(context.Context) error
	Close() error
	CloseChan() <-chan struct{}
}

// DataCarrier owns one transport-native session and its bounded stream
// permits.
// DataCarrierCloseReason says, in a finite vocabulary, why a data carrier ended. It is recorded
// once per carrier and written to the host log, so a stream cut can be tied to the carrier event
// that caused it.
type DataCarrierCloseReason string

const (
	// CloseHealthPingMissed: the pool's periodic health ping went unanswered.
	CloseHealthPingMissed DataCarrierCloseReason = "health_ping_missed"
	// CloseSessionClosed: the transport session closed (the peer or the network ended it).
	CloseSessionClosed DataCarrierCloseReason = "session_closed"
	// CloseSessionError: accepting a stream failed with a session error.
	CloseSessionError DataCarrierCloseReason = "session_error"
	// CloseContextDone: the carrier's own context ended.
	CloseContextDone DataCarrierCloseReason = "context_done"
	// CloseTargetRemoved: the edge target this carrier ran to is no longer assigned.
	CloseTargetRemoved DataCarrierCloseReason = "target_removed"
	// ClosePoolShutdown: the pool was closed or drained.
	ClosePoolShutdown DataCarrierCloseReason = "pool_shutdown"
	// ClosePoolDialFailed: the pool discarded a freshly dialed carrier (failed first ping or pool
	// already closed).
	ClosePoolDialFailed DataCarrierCloseReason = "pool_dial_failed"
	// CloseLocal: closed by the caller without a more specific reason.
	CloseLocal DataCarrierCloseReason = "local_close"
)

type DataCarrier struct {
	id        string
	ctx       context.Context
	session   DataCarrierSession
	config    DataCarrierConfig
	identity  DataCarrierIdentity
	admission *DataCarrierAdmission
	permits   chan struct{}
	openings  chan struct{}
	accepted  chan dataCarrierAcceptResult
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	active    atomic.Int64
	started   time.Time
	reason    DataCarrierCloseReason
}

type dataCarrierAcceptResult struct {
	stream *DataCarrierStream
	open   StreamOpen
	err    error
}
