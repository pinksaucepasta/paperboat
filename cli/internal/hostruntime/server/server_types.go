package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/admission"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

var (
	ErrInvalidConfiguration  = errors.New("invalid server configuration")
	ErrHandshakeRequired     = errors.New("hello required before requests")
	ErrCapabilityUnavailable = errors.New("capability unavailable")
	ErrServerStopped         = errors.New("server stopped")
	ErrStreamClosed          = errors.New("output stream closed")
)

var errTerminalStreamClosed = errors.New("terminal stream is closed")

type Connection interface {
	io.Reader
	io.Writer
	io.Closer
}

type FramedConnection interface {
	Connection
	WriteStructured(protocol.Frame) error
	WriteBinary(protocol.BinaryFrame) error
}

type ApplicationConnection interface {
	ReadApplication() (protocol.Frame, []byte, error)
	WriteTerminalOutput(uint32, protocol.BinaryFrame) error
}

type ProtocolCloser interface {
	CloseProtocol(int, string) error
}

type ConnectionRevocation interface {
	RevocationFlag() *atomic.Bool
}

type WelcomeAugmenter interface {
	AugmentWelcome(map[string]any)
}

type AuthorizationObserver interface {
	AuthorizationSucceeded()
}

// Authorizer must validate the operation credential and all applicable bindings.
// It runs before Handler, including for requests that name nonexistent resources.
type Authorizer interface {
	Authorize(context.Context, protocol.Frame) (Authorization, error)
}

type Authorization struct {
	WorkspaceID    string
	ActorAccountID string
	// JournalBinding is a stable, non-secret identity and resource binding. It is
	// included in idempotency hashing so operation IDs cannot cross principals.
	JournalBinding      string
	EnvironmentID       string
	MachineID           string
	SourceMachineID     string
	UserID              string
	AccountID           string
	ClientID            string
	SessionID           string
	TerminalRole        TerminalRole
	TerminalGeneration  uint64
	BrowserTerminal     bool
	BrowserInspector    bool
	BrowserAttachmentID string
	PolicyGeneration    uint64
	ResourceID          string
	OperationID         string
	RequestID           string
	RequestHash         string
	IdempotencyKey      string
	ExpiresAt           time.Time
	Revoked             *atomic.Bool
	RevokedSignal       <-chan struct{}
	Value               any
}

type TerminalRole string

const (
	TerminalRoleOwner       TerminalRole = "owner"
	TerminalRoleViewer      TerminalRole = "viewer"
	TerminalRoleInteractive TerminalRole = "interactive"
)

type AuthorizationCloser interface{ CloseAuthorization() }

type Handler interface {
	Handle(context.Context, Authorization, string, json.RawMessage) operation.Outcome
}

type OperationHandler interface {
	HandleOperation(context.Context, Authorization, string, string, json.RawMessage) operation.Outcome
}

type OutputStream interface {
	Next(context.Context) (protocol.BinaryFrame, error)
	Close() error
}

type StreamError struct {
	Code      string
	Details   json.RawMessage
	CloseCode int
	Cause     error
}

func (e *StreamError) Error() string { return e.Code }

func (e *StreamError) Unwrap() error { return e.Cause }

type StreamEnd struct {
	Payload json.RawMessage
}

func (e *StreamEnd) Error() string { return "output stream ended" }

type StreamHandler interface {
	OpenStream(context.Context, Authorization, string, json.RawMessage, operation.Outcome, bool) (OutputStream, bool, error)
}

type ControlHandler interface {
	HandleControl(context.Context, Authorization, protocol.Frame) operation.Outcome
}

type TerminalDataHandler interface {
	HandleTerminalACK(context.Context, Authorization, string, string, uint64) error
	HandleTerminalResize(context.Context, Authorization, string, string, uint16, uint16) error
}

// TerminalInputHandler receives one durable, idempotent input operation. The
// input sequence is part of the identity and must never be treated as a raw
// stream offset. Implementations return the recorded decision so the server
// can acknowledge accepted, rejected, uncertain, and conflicting deliveries.
type TerminalInputHandler interface {
	HandleTerminalInput(context.Context, Authorization, string, string, uint64, uint64, []byte) (session.InputDecision, error)
}

// legacyTerminalInputHandler is test-only compatibility for non-session data
// handlers. The production dispatcher implements TerminalInputHandler and
// never uses this path.
type legacyTerminalInputHandler interface {
	HandleTerminalInput(context.Context, Authorization, string, string, uint64, []byte) error
}

type ExecDataHandler interface {
	HandleExecInput(context.Context, Authorization, string, []byte) error
	HandleExecEOF(context.Context, Authorization, string) error
	HandleExecResize(context.Context, Authorization, string, uint16, uint16) error
}

type SSHDataHandler interface {
	HandleSSHInput(context.Context, Authorization, string, []byte) error
	HandleSSHEOF(context.Context, Authorization, string) error
}

type terminalStreamBinding struct {
	lease *admission.Lease
	stop  func() bool

	kind           string
	authorization  Authorization
	operationID    string
	sessionID      string
	attachmentID   string
	generation     uint64
	inputSequence  uint64
	resizeSequence uint64
	revoked        *atomic.Bool
}

type terminalConnectionState struct {
	ctx context.Context

	mu         sync.RWMutex
	nextID     uint32
	streams    map[uint32]*terminalStreamBinding
	closed     map[uint32]struct{}
	closedIDs  []uint32
	revoked    *atomic.Bool
	expired    chan struct{}
	expireOnce sync.Once
	done       <-chan struct{}
}

func newTerminalConnectionState(revoked ...*atomic.Bool) *terminalConnectionState {
	flag := new(atomic.Bool)
	if len(revoked) != 0 && revoked[0] != nil {
		flag = revoked[0]
	}
	return &terminalConnectionState{streams: make(map[uint32]*terminalStreamBinding), closed: make(map[uint32]struct{}), expired: make(chan struct{}), revoked: flag}
}

type Config struct {
	Negotiator        protocol.Negotiator
	Journal           *operation.Journal
	Authorizer        Authorizer
	Handler           Handler
	MaxConcurrent     int
	HeartbeatInterval time.Duration
	MutationDeadline  time.Duration
	Metrics           MetricRecorder
}

type MetricRecorder interface {
	Record(string, float64, map[string]string) error
}

type terminalMetricRecorder interface {
	RecordTerminalStage(string, time.Duration, int)
}
