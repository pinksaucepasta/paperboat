// Package resolver turns an enrolled machine into the information needed to
// connect through Paperboat's terminal transport.
package resolver

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

var ErrTerminalInputQueueFull = errors.New("terminal input queue is full")

type TerminalInput struct {
	Sequence uint64
	Data     []byte
}

// TerminalInputQueue is shared by all transport attempts for one terminal. It
// keeps input the host has not answered yet, so a reattach can deliver each
// keystroke exactly once: the host refuses a sequence it has passed and
// answers a repeat of its last one from its journal.
type TerminalInputQueue struct {
	mu      sync.Mutex
	next    uint64
	limit   int
	pending map[uint64]TerminalInput
}

func NewTerminalInputQueue(limit int) *TerminalInputQueue {
	if limit < 1 {
		limit = 256
	}
	return &TerminalInputQueue{limit: limit, pending: make(map[uint64]TerminalInput)}
}

func (q *TerminalInputQueue) Enqueue(data []byte) (uint64, error) {
	if q == nil || len(data) == 0 {
		return 0, ErrTerminalInputQueueFull
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) >= q.limit {
		return 0, ErrTerminalInputQueueFull
	}
	q.next++
	if q.next == 0 {
		return 0, ErrTerminalInputQueueFull
	}
	q.pending[q.next] = TerminalInput{Sequence: q.next, Data: append([]byte(nil), data...)}
	return q.next, nil
}

// Complete removes input the host has answered, whatever its status.
func (q *TerminalInputQueue) Complete(sequence uint64) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, sequence)
	if sequence > q.next {
		q.next = sequence
	}
}

// Reconcile settles pending input against the host's last input sequence after
// a reattach. Entries before it were decided by the host, but their answers
// were lost: they are returned as decided and never sent again. The rest are
// returned in order to send again, before any new input: the one at the host's
// sequence (the host answers it from its journal) and every later one.
func (q *TerminalInputQueue) Reconcile(hostSequence uint64) (decided, resend []TerminalInput) {
	if q == nil {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for sequence, item := range q.pending {
		if sequence < hostSequence {
			decided = append(decided, item)
			delete(q.pending, sequence)
		} else {
			resend = append(resend, item)
		}
	}
	slices.SortFunc(decided, func(a, b TerminalInput) int { return cmp.Compare(a.Sequence, b.Sequence) })
	slices.SortFunc(resend, func(a, b TerminalInput) int { return cmp.Compare(a.Sequence, b.Sequence) })
	first := q.next + 1
	if len(resend) > 0 {
		first = resend[0].Sequence
	}
	if first > hostSequence+1 {
		// The host does not know this numbering (a new attachment): what it never
		// answered cannot be matched, so it counts as decided, and numbering
		// continues from the host's sequence.
		for _, item := range resend {
			delete(q.pending, item.Sequence)
		}
		decided, resend = append(decided, resend...), nil
		q.next = hostSequence
	} else if hostSequence > q.next {
		q.next = hostSequence
	}
	return decided, resend
}

// ConnectRequest describes what the user asked to connect to.
type ConnectRequest struct {
	Machine string
	// Credential is the current Paperboat client-session access credential.
	Credential config.Credential
	// TerminalSessionID is the immutable server catalog ID. It is required for
	// terminal connections; pb creates a fresh session before resolving.
	TerminalSessionID string
	// ResolvedMachine carries a machine catalog entry the caller has already
	// resolved. When set, the resolver skips its own target lookup round trips
	// and proceeds directly to the connection descriptor.
	ResolvedMachine *ResolvedMachine
	// CreateTerminalSession asks the resolver to create a durable terminal
	// session and issue its connection descriptor in one round trip. It is
	// mutually exclusive with TerminalSessionID.
	CreateTerminalSession *TerminalSessionCreate
}

// ResolvedMachine is a machine catalog entry the caller resolved before
// requesting a connection descriptor.
type ResolvedMachine struct {
	ID         string
	Name       string
	State      string
	Generation uint64
}

// TerminalSessionCreate describes the durable terminal session the resolver
// should create while issuing the connection descriptor.
type TerminalSessionCreate struct {
	CWD            string
	Name           string
	IdempotencyKey string
}

// ConnectInfo is what the resolver hands back to the tunnel + session layers.
type ConnectInfo struct {
	Workspace string
	// TargetKind is "machine" for the enrolled machine resolved by the catalog.
	TargetKind        string
	MachineID         string
	Machine           string
	MachineState      string
	MachineGeneration uint64
	// TunnelTarget identifies how the tunnel layer should reach the helper.
	TunnelTarget string
	// Local is true when this resolves to a local development target.
	Local bool
	// Terminal is the helper WebSocket attach descriptor returned by paperboat-server's
	// pre-connect broker.
	Terminal     *TerminalTarget
	FileTransfer *FileTransferTarget
	// TerminalSession describes the durable session the resolver created or
	// selected. It is populated when the resolver created the session as part
	// of a create-and-connect round trip.
	TerminalSession *TerminalSessionInfo
}

// TerminalSessionInfo carries the durable terminal session the resolver
// created or selected.
type TerminalSessionInfo struct {
	ID             string
	Name           string
	EvictedSession *api.TerminalSession
}

// AuthTarget is short-lived, scoped auth material returned by the broker.
type AuthTarget struct {
	Method    string
	Ticket    string
	Token     string
	ExpiresAt string
	Scopes    []string
	// ResourceID is the authoritative machine-access grant shared by the
	// operation credential and signed native network scope.
	ResourceID     string
	UsageSessionID string
	// Offline opens the stream without a credential for the host to admit from its own signed
	// scopes, because Paperboat could not be reached (F21-11).
	Offline bool
}

// TerminalTarget is the client-safe environment WebSocket endpoint returned
// by the broker. It carries scoped terminal auth, not
// raw machine addresses, SSH credentials, or tunnel control tokens.
type TerminalTarget struct {
	Protocol      string
	EnvironmentID string
	QUICEndpoint  string
	WSSEndpoint   string
	Auth          AuthTarget
	SessionID     string
	CWD           string
	// Debug requests connection-scoped runtime diagnostics for the local
	// status bar. It is never sent to the control plane or remote shell.
	Debug bool
	// Env is local-terminal environment forwarded on attach (TERM, COLORTERM,
	// ...) so the remote PTY spawns with the client's terminal capabilities.
	// Applied by the Paperboat host runtime when the PTY (re)starts.
	Env map[string]string
	// Cols/Rows seed the remote PTY size at attach time so retained history
	// replays at the local geometry instead of the server default until the
	// first resize lands.
	Cols uint16
	Rows uint16
	// RestartIfNotRunning is true only for the initial user-requested attach.
	// Transport reconnects must observe an exited session and its final status.
	RestartIfNotRunning bool
	// ReplayHistory controls whether an attach should emit retained terminal
	// history. Reconnects suppress it because the local session already has it.
	ReplayHistory bool
	AfterSequence int
	SequenceSink  func(int)
	ReplayGapSink func(requested, earliest, latest uint64)
	// LaunchWarningSink receives launch notices from the target, such as
	// "environment_pending". The terminal started regardless.
	LaunchWarningSink func(codes []string)
	// InputAttachmentID is stable across transport reconnects. The host uses it
	// as part of the durable input idempotency key; it is never a transport
	// stream identifier and is safe to reuse after a detached stream is closed.
	InputAttachmentID string
	InputQueue        *TerminalInputQueue
	// InputOwner names one pb connect across redials (see localdaemon input owners); empty elsewhere.
	InputOwner string
}

// Shared reports a role-scoped session attachment. Runtime authorization remains
// authoritative; this metadata only selects the client interaction behavior.
func (t *TerminalTarget) Shared() bool {
	return t != nil && len(t.Auth.Scopes) == 1 && (t.Auth.Scopes[0] == "terminal:view" || t.Auth.Scopes[0] == "terminal:control")
}

func (t *TerminalTarget) ViewOnly() bool {
	return t != nil && len(t.Auth.Scopes) == 1 && t.Auth.Scopes[0] == "terminal:view"
}

type FileTransferTarget struct {
	Endpoint             string
	SourceMachineID      string
	DestinationMachineID string
	InitiatingUserID     string
	Auth                 AuthTarget
	Policy               api.FileTransferPolicy
}

// MachineResolver resolves an enrolled machine to connect info.
type MachineResolver interface {
	Resolve(ctx context.Context, req ConnectRequest) (ConnectInfo, error)
}
