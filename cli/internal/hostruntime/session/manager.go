package session

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	xterm "github.com/gitpod-io/xterm-go"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

var (
	ErrSessionExists    = errors.New("session name already exists")
	ErrSessionUnknown   = errors.New("session not found")
	ErrSessionRunning   = errors.New("session is running")
	ErrInvalidSession   = errors.New("invalid session")
	ErrManagerStopped   = errors.New("session manager stopped")
	ErrResourceLimit    = errors.New("session resource limit")
	ErrUpdateBusy       = errors.New("session update blocked by live PTY")
	ErrUpdateInProgress = errors.New("session update already in progress")
)

// initialReplayBytes bounds attach replay to a terminal-sized recent tail while
// reserving attachment queue capacity for output that arrives during setup.
const initialReplayBytes uint64 = 64 << 10

const browserScreenScrollbackLines = 200

type PTYProcess interface {
	io.Reader
	InputWriter
	Resize(pty.Dimensions) error
	Signal(pty.Signal) error
	Wait(context.Context) (pty.ExitResult, error)
	Terminate(context.Context, time.Duration) (pty.ExitResult, error)
	CloseIO() error
}

type Launcher func(pty.Command) (PTYProcess, error)
type ContextLauncher func(context.Context, pty.Command) (PTYProcess, error)

type ManagerConfig struct {
	Launch             Launcher
	LaunchContext      ContextLauncher
	Random             io.Reader
	HistoryBytes       uint64
	AttachmentBytes    uint64
	TerminationTimeout time.Duration
	TerminationGrace   time.Duration
	MaxSessions        int
	Store              *store.Store
	MaxAttachments     int
	MaxInputDecisions  int
	RecoveryExitSignal string
	// PreserveStoppedHistory is set by the private owner runtime when it has
	// confirmed that the store is being handed to a replacement owner. Ordinary
	// recovery keeps the existing stopped-output cleanup behavior.
	PreserveStoppedHistory bool
	Metrics                interface {
		Record(string, float64, map[string]string) error
	}
}

type Manager struct {
	writerOnce        sync.Once
	output            *outputWriter
	admission         sync.RWMutex
	mu                sync.RWMutex
	config            ManagerConfig
	sessions          map[string]*managedSession
	names             map[string]string
	creating          map[string]string // ID/name reservations while launch and persistence run
	stopping          bool
	retiring          bool
	retired           bool
	updateTransaction string
}

type managedSession struct {
	opMu             sync.Mutex // in-memory state only; never held across PTY or store I/O
	operationMu      sync.Mutex // lifecycle and durable metadata ordering
	resizeMu         sync.Mutex // PTY resize ordering, independent of lifecycle
	outputMu         sync.Mutex // capture and clear boundaries
	inputMu          sync.Mutex
	persistMu        sync.Mutex
	liveProcess      atomic.Pointer[liveProcess]
	interrupt        atomic.Pointer[InputKey] // the interrupt key Interrupt delivered ahead of stuck input
	id               string
	name             string
	supportReference atomic.Pointer[string]
	command          pty.Command
	lifecycle        *Lifecycle
	persistedState   State // Last successfully committed lifecycle state; guarded by opMu.
	history          *history.History
	fanout           *Fanout
	inputs           *InputJournal
	process          PTYProcess
	exit             *pty.ExitResult
	sizes            *windowSizes // which attached window sets the PTY size (F3-17)
	sizeApplying     atomic.Bool  // an asynchronous size application is queued or running
	sizeDirty        atomic.Bool  // typing moved control to another window since the last application
	// persisting and persistedLatest are owned by persistMu: whether the group-commit writer
	// still persists this session's output, and how far the store holds it.
	persisting      bool
	persistedLatest uint64
	persistErr      error
	modes           terminalModeTracker
	identification  identificationTracker
	screen          *xterm.Terminal
	continuation    screenContinuation
	participants    map[string]Participant
}

// liveProcess is immutable after publication. Terminal v1 input can therefore
// resolve its generation and PTY without waiting for lifecycle or persistence.
type liveProcess struct {
	process    PTYProcess
	generation uint64
}

type CreateRequest struct {
	ID      string
	Name    string
	Command pty.Command
}

type Snapshot struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	CWD               string          `json:"cwd"`
	Title             string          `json:"title"`
	CurrentDirectory  string          `json:"current_directory,omitempty"`
	ForegroundProcess string          `json:"foreground_process,omitempty"`
	Dimensions        pty.Dimensions  `json:"dimensions"`
	State             State           `json:"state"`
	Generation        uint64          `json:"generation"`
	EarliestSequence  uint64          `json:"earliest_sequence"`
	LatestSequence    uint64          `json:"latest_sequence"`
	Exit              *pty.ExitResult `json:"exit,omitempty"`
	TerminalModes     TerminalModes   `json:"terminal_modes"`
	Participants      []Participant   `json:"participants,omitempty"`
}

type Participant struct {
	AttachmentID string    `json:"attachment_id"`
	AccountID    string    `json:"account_id"`
	ClientID     string    `json:"client_id"`
	Role         string    `json:"role"`
	ConnectedAt  time.Time `json:"connected_at"`
	Browser      bool      `json:"-"`
	// Dimensions is the window's size when it attached; 0x0 until it reports one.
	Dimensions pty.Dimensions `json:"-"`
}

type AttachResult struct {
	Snapshot        Snapshot       `json:"snapshot"`
	Replay          history.Replay `json:"replay"`
	FirstAttachment bool           `json:"first_attachment"`
}

func (m *Manager) ResourceCounts() map[string]uint64 {
	m.mu.RLock()
	sessions := make([]*managedSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	counts := map[string]uint64{"sessions": uint64(len(sessions))}
	for _, session := range sessions {
		session.opMu.Lock()
		counts["attachments"] += uint64(session.fanout.Count())
		if session.process != nil {
			counts["processes"]++
		}
		session.opMu.Unlock()
	}
	return counts
}

func (m *Manager) checkRetiring() error {
	m.mu.RLock()
	retiring := m.retiring
	m.mu.RUnlock()
	if retiring {
		return ErrManagerStopped
	}
	return nil
}

func NewManager(config ManagerConfig) (*Manager, error) {
	if config.Launch == nil && config.LaunchContext == nil {
		return nil, ErrInvalidSession
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.HistoryBytes == 0 {
		config.HistoryBytes = helperconfig.DefaultResources.HistoryBytes
	}
	if config.AttachmentBytes == 0 {
		config.AttachmentBytes = 1 << 20
	}
	if config.TerminationTimeout == 0 {
		config.TerminationTimeout = 10 * time.Second
	}
	if config.TerminationGrace == 0 {
		config.TerminationGrace = 2 * time.Second
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = helperconfig.DefaultResources.MaxSessions
	}
	if config.MaxAttachments == 0 {
		config.MaxAttachments = helperconfig.DefaultResources.MaxAttachments
	}
	if config.MaxInputDecisions == 0 {
		config.MaxInputDecisions = helperconfig.DefaultResources.MaxInputDecisions
	}
	if config.RecoveryExitSignal == "" {
		config.RecoveryExitSignal = "runtime_restart"
	}
	if config.HistoryBytes < 1 || config.AttachmentBytes < 1 || config.TerminationTimeout <= 0 || config.TerminationGrace < 0 || config.TerminationGrace > config.TerminationTimeout || config.MaxSessions < 1 || config.MaxAttachments < 1 || config.MaxInputDecisions < 1 || config.RecoveryExitSignal != "runtime_restart" && config.RecoveryExitSignal != "machine_reboot" {
		return nil, ErrInvalidSession
	}
	manager := &Manager{config: config, sessions: make(map[string]*managedSession), names: make(map[string]string), creating: make(map[string]string)}
	if config.Store != nil {
		if err := manager.recover(context.Background()); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func (m *Manager) launch(ctx context.Context, command pty.Command) (PTYProcess, error) {
	if ctx == nil {
		return nil, ErrInvalidSession
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var process PTYProcess
	var err error
	if m.config.LaunchContext != nil {
		process, err = m.config.LaunchContext(ctx, command)
	} else {
		process, err = m.config.Launch(command)
	}
	if errors.Is(err, pty.ErrInvalidCommand) || errors.Is(err, pty.ErrInvalidCWD) || errors.Is(err, pty.ErrInvalidDimensions) {
		return process, err
	}
	return process, classifySessionFailure("command", err)
}

// liveSessionsLocked counts the sessions that can still hold a process. Closed records are
// retained only for display and must not consume the live-session limit, matching the server
// (terminal_session_limit_reached) and the session-owner router.
func (m *Manager) liveSessionsLocked() int {
	live := len(m.creating)
	for _, current := range m.sessions {
		if state, _ := current.lifecycle.Snapshot(); state != Closed && state != Deleted {
			live++
		}
	}
	return live
}

// reserveCreate keeps in-flight launches in the name, ID and capacity checks
// without publishing a partially initialized session or holding mu over I/O.
func (m *Manager) reserveCreate(request CreateRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return "", ErrManagerStopped
	}
	if m.liveSessionsLocked() >= m.config.MaxSessions {
		return "", ErrResourceLimit
	}
	if _, exists := m.names[request.Name]; exists {
		return "", ErrSessionExists
	}
	id := request.ID
	if id != "" {
		if _, exists := m.sessions[id]; exists || m.creating[id] != "" {
			return "", ErrSessionExists
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		if id != "" {
			break
		}
		candidate, err := randomID(m.config.Random)
		if err != nil {
			return "", err
		}
		if _, exists := m.sessions[candidate]; !exists && m.creating[candidate] == "" {
			id = candidate
			break
		}
	}
	if id == "" {
		return "", ErrSessionExists
	}
	m.creating[id], m.names[request.Name] = request.Name, id
	return id, nil
}

func (m *Manager) Create(ctx context.Context, request CreateRequest) (Snapshot, error) {
	if !validSessionName(request.Name) {
		return Snapshot{}, ErrInvalidSession
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.admission.RLock()
	defer m.admission.RUnlock()
	if m.updateTransaction != "" {
		return Snapshot{}, ErrUpdateInProgress
	}
	id, err := m.reserveCreate(request)
	if err != nil {
		return Snapshot{}, err
	}
	published := false
	defer func() {
		m.mu.Lock()
		delete(m.creating, id)
		if !published {
			delete(m.names, request.Name)
		}
		m.mu.Unlock()
	}()
	retained, _ := history.New(m.config.HistoryBytes)
	session := &managedSession{id: id, name: request.Name, command: request.Command, lifecycle: NewLifecycle(), persistedState: Creating, history: retained, fanout: NewFanout(), participants: make(map[string]Participant), sizes: newWindowSizes(), screen: xterm.New(xterm.WithCols(int(request.Command.Dimensions.Columns)), xterm.WithRows(int(request.Command.Dimensions.Rows)), xterm.WithScrollback(browserScreenScrollbackLines))}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	setSessionSupportReference(session, supportref.FromContext(ctx))
	if m.config.Store != nil {
		if err := m.config.Store.CreateSession(ctx, store.Session{ID: id, Name: request.Name, CWD: request.Command.CWD, CommandPath: request.Command.Path, CommandArgs: request.Command.Args, CommandEnv: request.Command.Env, Columns: request.Command.Dimensions.Columns, Rows: request.Command.Dimensions.Rows, State: string(Creating), Generation: 0}); err != nil {
			return Snapshot{}, err
		}
	}
	process, err := m.launch(ctx, request.Command)
	if err != nil {
		_ = session.lifecycle.Transition(Closed)
		if cleanupErr := m.discardFailedCreate(ctx, session, Creating); cleanupErr != nil {
			observeSessionFailure(session, "lifecycle", cleanupErr)
		}
		return Snapshot{}, err
	}
	if err := session.lifecycle.Transition(Running); err != nil {
		closeErr := process.CloseIO()
		observeSessionFailure(session, "lifecycle", closeErr)
		return Snapshot{}, classifySessionFailure("lifecycle", errors.Join(err, closeErr))
	}
	_, generation := session.lifecycle.Snapshot()
	if err := m.persist(ctx, session); err != nil {
		terminateCtx, cancel := context.WithTimeout(context.Background(), m.config.TerminationTimeout)
		_, terminateErr := process.Terminate(terminateCtx, 0)
		cancel()
		cleanupErr := m.discardFailedCreate(context.Background(), session, Creating)
		observeSessionFailure(session, "lifecycle", errors.Join(terminateErr, cleanupErr))
		return Snapshot{}, errors.Join(err, classifySessionFailure("lifecycle", errors.Join(terminateErr, cleanupErr)))
	}
	session.inputs = NewBoundedInputJournal(generation, m.config.MaxInputDecisions)
	session.process = process
	session.identification = identificationTracker{}
	session.liveProcess.Store(&liveProcess{process: process, generation: generation})
	m.startOutputPersistence(session)
	m.mu.Lock()
	m.sessions[id] = session
	delete(m.creating, id)
	published = true
	m.mu.Unlock()
	snapshot := session.snapshotLocked()
	go m.capture(session, process)
	return snapshot, nil
}

// removeParticipantLocked forgets a detached attachment: its participant entry and its history
// acknowledgement. session.opMu must be held.
func (s *managedSession) removeParticipantLocked(attachmentID string) {
	delete(s.participants, attachmentID)
	s.sizes.detach(attachmentID, time.Now())
	if s.history != nil {
		s.history.Forget(attachmentID)
	}
}
