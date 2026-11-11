// Package recordings records Paperboat terminal sessions on the host: output and size changes of a
// session go, without ever blocking the PTY loop, into encrypted segment files (internal/recording)
// in an owner-only directory, bounded by a disk quota and a retention period.
package recordings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/recording"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

const (
	TriggerRule   = wire.TriggerRule
	TriggerManual = wire.TriggerManual

	ReasonStopped      = wire.ReasonStopped
	ReasonSessionEnded = wire.ReasonSessionEnded
	ReasonQuota        = wire.ReasonQuota
	ReasonPolicy       = wire.ReasonPolicy
	ReasonError        = wire.ReasonError // also a recording its host process never closed
	ReasonRetention    = wire.ReasonRetention

	MaxAccounts = wire.MaxAccounts
	metaFile    = "meta.json"
)

var (
	ErrRecording    = errors.New("the session is already recorded")
	ErrNotRecording = errors.New("the session is not recorded")
	ErrNotFound     = errors.New("recording not found")
	ErrInvalid      = errors.New("invalid recording request")
	ErrStopped      = errors.New("recordings service stopped")
)

type Config struct {
	Root string
	// SegmentBytes and SegmentAge close a segment, whichever comes first; they bound what a crash loses.
	SegmentBytes int
	SegmentAge   time.Duration
	// QueueBytes bounds the output one recording holds in memory; beyond it output is dropped and the
	// recording gets a gap marker, so a stuck disk never slows the terminal.
	QueueBytes int64
	Now        func() time.Time
}

// Key is the team recording public key of one generation.
type Key struct {
	Generation int64
	Public     []byte
}

type StartRequest struct {
	Trigger        string
	ActorAccountID string
	Accounts       []string
	Key            Key
	QuotaBytes     int64
}

// State is what attached clients show: the REC indicator and who may stop it (the shared contract).
type State = wire.State

// Meta is a recording's plaintext index entry, next to its encrypted segments.
type Meta struct {
	ID            string     `json:"id"`
	SessionID     string     `json:"session_id"`
	Trigger       string     `json:"trigger"`
	StartedBy     string     `json:"started_by,omitempty"`
	StoppedBy     string     `json:"stopped_by,omitempty"`
	Accounts      []string   `json:"accounts"`
	KeyGeneration int64      `json:"key_generation"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	EndReason     string     `json:"end_reason,omitempty"`
	Bytes         int64      `json:"bytes"`
	Segments      uint32     `json:"segments"`
}

type Service struct {
	config Config
	mu     sync.RWMutex
	active map[string]*recorder // by terminal session id
	used   int64                // bytes of every segment on disk, guarded by mu
	closed bool
	// pruneMu serializes deletions, so two prunes never delete (and release) one recording twice.
	pruneMu sync.Mutex
}

func Open(config Config) (*Service, error) {
	if !filepath.IsAbs(config.Root) {
		return nil, ErrInvalid
	}
	if config.SegmentBytes <= 0 || config.SegmentBytes > recording.MaxSegmentPlaintext/2 {
		config.SegmentBytes = 1 << 20
	}
	if config.SegmentAge <= 0 {
		config.SegmentAge = time.Minute
	}
	if config.QueueBytes <= 0 {
		config.QueueBytes = 8 << 20
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := store.EnsurePrivateDirectory(config.Root); err != nil {
		return nil, fmt.Errorf("prepare the recordings directory: %w", err)
	}
	s := &Service{config: config, active: make(map[string]*recorder)}
	metas, err := s.scan(true)
	if err != nil {
		return nil, err
	}
	for _, meta := range metas {
		s.used += meta.Bytes
	}
	return s, nil
}

func (s *Service) dir(id string) string { return filepath.Join(s.config.Root, id) }

func (s *Service) writeMeta(meta Meta) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.dir(meta.ID), metaFile), encoded, atomicfile.CurrentOwnerOptions(0o600))
}

func readMeta(path string) (Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if len(data) > 64<<10 || json.Unmarshal(data, &meta) != nil || !wire.ValidRecordingID(meta.ID) {
		return Meta{}, ErrInvalid
	}
	return meta, nil
}

// scan reads every recording's index entry. At Open (cleanup), a directory without one is a recording
// whose start failed half way, and is removed; later it may be a start in progress, and is skipped.
// shortcut: Open's cleanup can remove a start another process has in progress at that instant; the
// start then fails on its first segment and reports an error. Fine while starts are rare.
func (s *Service) scan(cleanup bool) ([]Meta, error) {
	entries, err := os.ReadDir(s.config.Root)
	if err != nil {
		return nil, fmt.Errorf("read the recordings directory: %w", err)
	}
	var metas []Meta
	for _, entry := range entries {
		if !entry.IsDir() || !wire.ValidRecordingID(entry.Name()) {
			continue
		}
		meta, err := readMeta(filepath.Join(s.dir(entry.Name()), metaFile))
		if err != nil || meta.ID != entry.Name() {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrInvalid) || err == nil {
				if cleanup {
					_ = os.RemoveAll(s.dir(entry.Name()))
				}
				continue
			}
			return nil, fmt.Errorf("read recording %s: %w", entry.Name(), err)
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// Prepared is a recording whose directory, index entry and data key exist but which does not yet
// receive output. Prepare does the disk work, so Activate can run under the session's output lock.
type Prepared struct {
	meta    Meta
	dataKey []byte
	sealed  recording.SealedKey
	quota   int64
}

func (s *Service) Prepare(sessionID string, request StartRequest) (*Prepared, error) {
	if sessionID == "" || (request.Trigger != TriggerRule && request.Trigger != TriggerManual) || request.Trigger == TriggerManual && request.ActorAccountID == "" || request.Key.Generation < 1 || request.QuotaBytes <= 0 {
		return nil, ErrInvalid
	}
	s.mu.RLock()
	_, busy := s.active[sessionID]
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return nil, ErrStopped
	}
	if busy {
		return nil, ErrRecording
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	dataKey, err := recording.NewDataKey()
	if err != nil {
		return nil, err
	}
	p := &Prepared{dataKey: dataKey, quota: request.QuotaBytes, meta: Meta{
		ID: "rec_" + id.String(), SessionID: sessionID, Trigger: request.Trigger, StartedBy: request.ActorAccountID,
		KeyGeneration: request.Key.Generation, StartedAt: s.config.Now().UTC(),
	}}
	for _, account := range request.Accounts {
		p.meta.Accounts = addAccount(p.meta.Accounts, account)
	}
	if p.sealed, err = recording.SealDataKey(request.Key.Public, p.meta.ID, request.Key.Generation, dataKey); err != nil {
		return nil, fmt.Errorf("seal the recording key: %w", err)
	}
	if err := store.EnsurePrivateDirectory(s.dir(p.meta.ID)); err != nil {
		return nil, fmt.Errorf("create the recording directory: %w", err)
	}
	if err := s.writeMeta(p.meta); err != nil {
		_ = os.RemoveAll(s.dir(p.meta.ID))
		return nil, fmt.Errorf("write the recording index: %w", err)
	}
	return p, nil
}

// Abort removes a prepared recording that was never activated.
func (s *Service) Abort(p *Prepared) { _ = os.RemoveAll(s.dir(p.meta.ID)) }

// Activate starts receiving output. screen is the session's current screen, the recording's first
// output, so a recording started mid-session replays from what people saw. It does no disk work.
func (s *Service) Activate(p *Prepared, columns, rows uint16, screen []byte) (State, error) {
	header, err := recording.HeaderLine(recording.Header{
		Width: int(columns), Height: int(rows), Timestamp: p.meta.StartedAt.Unix(), Env: map[string]string{"TERM": "xterm-256color"},
		Paperboat: recording.HeaderInfo{Recording: p.meta.ID, Session: p.meta.SessionID, Trigger: p.meta.Trigger, StartedBy: p.meta.StartedBy},
	})
	if err != nil {
		return State{}, err
	}
	r := newRecorder(s, p, header)
	if len(screen) > 0 {
		r.appendOutput(p.meta.StartedAt, screen)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrStopped
	}
	if _, busy := s.active[p.meta.SessionID]; busy {
		return State{}, ErrRecording
	}
	s.active[p.meta.SessionID] = r
	go r.run()
	return r.state(), nil
}

func (s *Service) lookup(sessionID string) *recorder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active[sessionID]
}

// Output hands a session's PTY output to its recording, if any. It never blocks.
func (s *Service) Output(sessionID string, at time.Time, data []byte) {
	if r := s.lookup(sessionID); r != nil {
		r.enqueue(event{at: at, code: recording.CodeOutput, data: data})
	}
}

// Resize records a session's new PTY size, if it is recorded. It never blocks.
func (s *Service) Resize(sessionID string, at time.Time, columns, rows uint16) {
	if r := s.lookup(sessionID); r != nil {
		r.enqueue(event{at: at, code: recording.CodeResize, data: []byte(recording.ResizeData(columns, rows))})
	}
}

// Join names a person who attached to a recorded session in its recording.
func (s *Service) Join(sessionID, accountID string) {
	if r := s.lookup(sessionID); r != nil && accountID != "" {
		r.mu.Lock()
		r.meta.Accounts = addAccount(r.meta.Accounts, accountID)
		r.mu.Unlock()
	}
}

func addAccount(accounts []string, account string) []string {
	if account == "" || len(accounts) >= MaxAccounts {
		return accounts
	}
	for _, existing := range accounts {
		if existing == account {
			return accounts
		}
	}
	return append(accounts, account)
}

// State reports a session's recording for its attached clients; nil when it is not recorded.
func (s *Service) State(sessionID string) *State {
	if r := s.lookup(sessionID); r != nil {
		state := r.state()
		return &state
	}
	return nil
}

// Stop ends a session's recording, writes its last segment and returns its index entry.
func (s *Service) Stop(sessionID, actorAccountID, reason string) (Meta, error) {
	s.mu.Lock()
	r := s.active[sessionID]
	delete(s.active, sessionID)
	s.mu.Unlock()
	if r == nil {
		return Meta{}, ErrNotRecording
	}
	return r.finish(actorAccountID, reason), nil
}

// Shutdown ends every recording as its session goes with the host process.
func (s *Service) Shutdown() []Meta {
	s.mu.Lock()
	s.closed = true
	active := s.active
	s.active = map[string]*recorder{}
	s.mu.Unlock()
	var ended []Meta
	for _, r := range active {
		ended = append(ended, r.finish("", ReasonSessionEnded))
	}
	return ended
}

// List returns every recording on this machine, newest first.
func (s *Service) List() ([]Meta, error) {
	metas, err := s.scan(false)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	for i := range metas {
		if r := s.active[metas[i].SessionID]; r != nil && r.meta.ID == metas[i].ID {
			metas[i] = r.snapshot()
		}
	}
	s.mu.RUnlock()
	sort.Slice(metas, func(i, j int) bool { return metas[i].StartedAt.After(metas[j].StartedAt) })
	return metas, nil
}

// CloseInterrupted ends a recording whose host process stopped without closing it; the caller has
// checked that its session no longer runs.
func (s *Service) CloseInterrupted(meta Meta) (Meta, error) {
	if meta.EndedAt != nil || !wire.ValidRecordingID(meta.ID) {
		return meta, ErrInvalid
	}
	now := s.config.Now().UTC()
	meta.EndedAt, meta.EndReason = &now, ReasonError
	return meta, s.writeMeta(meta)
}

func segmentName(index uint32) string { return fmt.Sprintf("%06d.seg", index) }

// ReadSegment returns one encrypted segment file.
func (s *Service) ReadSegment(id string, index uint32) ([]byte, error) {
	if !wire.ValidRecordingID(id) {
		return nil, ErrNotFound
	}
	file, err := os.Open(filepath.Join(s.dir(id), segmentName(index)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > recording.MaxSegmentFile {
		return nil, ErrInvalid
	}
	data := make([]byte, info.Size())
	if _, err := file.ReadAt(data, 0); err != nil {
		return nil, err
	}
	return data, nil
}

// Prune deletes finished recordings older than retention, then the oldest finished ones while the
// directory holds more than quotaBytes. It returns what it deleted.
func (s *Service) Prune(retention time.Duration, quotaBytes int64) ([]Meta, error) {
	if retention <= 0 || quotaBytes <= 0 {
		return nil, ErrInvalid
	}
	return s.prune(s.config.Now().Add(-retention), quotaBytes, "")
}

func (s *Service) prune(cutoff time.Time, quotaBytes int64, keep string) ([]Meta, error) {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	metas, err := s.scan(false)
	if err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].StartedAt.Before(metas[j].StartedAt) })
	var deleted []Meta
	for _, meta := range metas {
		if meta.EndedAt == nil || meta.ID == keep {
			continue // still recording
		}
		// s.mu is never held across disk work: Output takes it on the PTY loop.
		s.mu.RLock()
		over := s.used > quotaBytes
		s.mu.RUnlock()
		reason := ""
		if meta.EndedAt.Before(cutoff) {
			reason = ReasonRetention
		} else if over {
			reason = ReasonQuota
		} else {
			continue
		}
		if err := os.RemoveAll(s.dir(meta.ID)); err != nil {
			return deleted, fmt.Errorf("delete recording %s: %w", meta.ID, err)
		}
		s.release(meta.Bytes)
		meta.EndReason = reason
		deleted = append(deleted, meta)
	}
	return deleted, nil
}

// reserve accounts a segment of size bytes against quota, deleting the oldest finished recordings
// first when needed; false means the quota is full of the active recordings themselves.
func (s *Service) reserve(owner string, size, quota int64) bool {
	if size > quota {
		return false
	}
	s.mu.Lock()
	over := s.used+size > quota
	s.mu.Unlock()
	if over {
		_, _ = s.prune(time.Time{}, quota-size, owner)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+size > quota {
		return false
	}
	s.used += size
	return true
}

func (s *Service) release(size int64) {
	s.mu.Lock()
	s.used -= size
	s.mu.Unlock()
}

// selfEnded removes a recording that ended on its own (quota, disk error) from the active set.
func (s *Service) selfEnded(r *recorder) {
	s.mu.Lock()
	if s.active[r.meta.SessionID] == r {
		delete(s.active, r.meta.SessionID)
	}
	s.mu.Unlock()
}
