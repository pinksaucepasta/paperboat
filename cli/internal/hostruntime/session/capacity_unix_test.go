//go:build darwin || linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

type capacityProcess struct {
	done   chan struct{}
	once   sync.Once
	exited *atomic.Int32
}

func (p *capacityProcess) Read([]byte) (int, error) { <-p.done; return 0, io.EOF }
func (p *capacityProcess) Write(data []byte) (int, error) {
	select {
	case <-p.done:
		return 0, io.ErrClosedPipe
	default:
		return len(data), nil
	}
}
func (*capacityProcess) Resize(pty.Dimensions) error { return nil }
func (*capacityProcess) Signal(pty.Signal) error     { return nil }
func (p *capacityProcess) Wait(ctx context.Context) (pty.ExitResult, error) {
	select {
	case <-p.done:
		return pty.ExitResult{Code: 0, ExitedAt: time.Now().UTC()}, nil
	case <-ctx.Done():
		return pty.ExitResult{}, ctx.Err()
	}
}
func (p *capacityProcess) Terminate(ctx context.Context, _ time.Duration) (pty.ExitResult, error) {
	p.once.Do(func() { close(p.done); p.exited.Add(1) })
	return p.Wait(ctx)
}
func (p *capacityProcess) CloseIO() error {
	p.once.Do(func() { close(p.done); p.exited.Add(1) })
	return nil
}

func TestConfiguredSessionCapacityRemainsBoundedAndShutsDownCleanly(t *testing.T) {
	const maxSessions, maxAttachments, maxDecisions = 32, 4, 10
	var launched, exited atomic.Int32
	manager, err := NewManager(ManagerConfig{
		Launch: func(pty.Command) (PTYProcess, error) {
			launched.Add(1)
			return &capacityProcess{done: make(chan struct{}), exited: &exited}, nil
		},
		MaxSessions: maxSessions, MaxAttachments: maxAttachments, MaxInputDecisions: maxDecisions,
		HistoryBytes: 1024, AttachmentBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	var first Snapshot
	for index := 0; index < maxSessions; index++ {
		created, err := manager.Create(context.Background(), CreateRequest{Name: fmt.Sprintf("session-%02d", index), Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
		if err != nil {
			t.Fatalf("create %d: %v", index, err)
		}
		if index == 0 {
			first = created
		}
	}
	if _, err := manager.Create(context.Background(), CreateRequest{Name: "overflow", Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("session limit err=%v", err)
	}
	for index := 0; index < maxAttachments; index++ {
		if _, err := manager.Attach(first.ID, fmt.Sprintf("att-%d", index), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Attach(first.ID, "att-overflow", 0); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("attachment limit err=%v", err)
	}
	counts := manager.ResourceCounts()
	if counts["sessions"] != maxSessions || counts["processes"] != maxSessions || counts["attachments"] != maxAttachments {
		t.Fatalf("resource counts = %v", counts)
	}
	for index := 0; index < maxDecisions; index++ {
		key := InputKey{ClientID: "cli", AttachmentID: "att-0", Generation: first.Generation, InputID: fmt.Sprintf("input-%d", index)}
		if decision, err := manager.Write(first.ID, key, []byte("x")); err != nil || decision.Status != InputAccepted {
			t.Fatalf("input %d decision=%#v err=%v", index, decision, err)
		}
	}
	overflow := InputKey{ClientID: "cli", AttachmentID: "att-0", Generation: first.Generation, InputID: "input-overflow"}
	if _, err := manager.Write(first.ID, overflow, []byte("x")); !errors.Is(err, ErrInputJournalFull) {
		t.Fatalf("decision limit err=%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for exited.Load() != maxSessions {
		select {
		case <-deadline.C:
			t.Fatalf("launched=%d exited=%d", launched.Load(), exited.Load())
		case <-ticker.C:
		}
	}
	if launched.Load() != maxSessions {
		t.Fatalf("launched=%d", launched.Load())
	}
}

func TestDeleteRunningSessionTerminatesAndReleasesCapacity(t *testing.T) {
	var exited atomic.Int32
	manager, err := NewManager(ManagerConfig{
		Launch: func(pty.Command) (PTYProcess, error) {
			return &capacityProcess{done: make(chan struct{}), exited: &exited}, nil
		},
		MaxSessions: 1, MaxAttachments: 1, MaxInputDecisions: 1,
		HistoryBytes: 1024, AttachmentBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Name: "running", Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if exited.Load() != 1 || manager.ResourceCounts()["sessions"] != 0 {
		t.Fatalf("exited=%d resources=%v", exited.Load(), manager.ResourceCounts())
	}
	if _, err := manager.Create(context.Background(), CreateRequest{Name: "replacement", Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}}); err != nil {
		t.Fatalf("replacement create: %v", err)
	}
}

func TestDeleteRunningSessionPurgesPersistentState(t *testing.T) {
	state, err := store.Open(context.Background(), store.Config{Root: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var exited atomic.Int32
	manager, err := NewManager(ManagerConfig{
		Store: state,
		Launch: func(pty.Command) (PTYProcess, error) {
			return &capacityProcess{done: make(chan struct{}), exited: &exited}, nil
		},
		MaxSessions: 20, MaxAttachments: 1, MaxInputDecisions: 1,
		HistoryBytes: 1024, AttachmentBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Name: "persistent", Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	records, err := state.Sessions(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("persistent sessions=%v err=%v", records, err)
	}
}

// Closed records are kept for display; only live sessions count against the limit, as on the
// server, so a user with 19 live sessions and a few closed ones can still open the 20th.
func TestClosedSessionsDoNotConsumeTheLiveSessionLimit(t *testing.T) {
	const maxSessions = 4
	var exited atomic.Int32
	manager, err := NewManager(ManagerConfig{
		Launch: func(pty.Command) (PTYProcess, error) {
			return &capacityProcess{done: make(chan struct{}), exited: &exited}, nil
		},
		MaxSessions: maxSessions, MaxAttachments: 2, MaxInputDecisions: 2,
		HistoryBytes: 1024, AttachmentBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) (Snapshot, error) {
		return manager.Create(context.Background(), CreateRequest{Name: name, Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	}
	var created []Snapshot
	for index := 0; index < maxSessions; index++ {
		snapshot, err := create(fmt.Sprintf("live-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, snapshot)
	}
	if _, err := create("overflow"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("limit err=%v", err)
	}
	if _, err := manager.Close(context.Background(), created[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := create("after-close"); err != nil {
		t.Fatalf("a closed record still consumed the live-session limit: %v", err)
	}
	if _, err := create("overflow-again"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("limit not enforced for live sessions: %v", err)
	}
}

// floodProcess prints a chunk every millisecond until it is terminated, like `tail -f` on a
// busy log.
type floodProcess struct {
	done chan struct{}
	once sync.Once
}

func (p *floodProcess) Read(buffer []byte) (int, error) {
	select {
	case <-p.done:
		return 0, io.EOF
	case <-time.After(time.Millisecond):
	}
	n := min(len(buffer), 512)
	for i := range buffer[:n] {
		buffer[i] = 'x'
	}
	return n, nil
}
func (*floodProcess) Write(data []byte) (int, error) { return len(data), nil }
func (*floodProcess) Resize(pty.Dimensions) error    { return nil }
func (*floodProcess) Signal(pty.Signal) error        { return nil }
func (p *floodProcess) Wait(ctx context.Context) (pty.ExitResult, error) {
	select {
	case <-p.done:
		return pty.ExitResult{ExitedAt: time.Now().UTC()}, nil
	case <-ctx.Done():
		return pty.ExitResult{}, ctx.Err()
	}
}
func (p *floodProcess) Terminate(ctx context.Context, _ time.Duration) (pty.ExitResult, error) {
	p.once.Do(func() { close(p.done) })
	return p.Wait(ctx)
}
func (p *floodProcess) CloseIO() error { p.once.Do(func() { close(p.done) }); return nil }

// Lane W load test on ubuntu24: with 100 busy terminals every output chunk was its own fsync'd
// transaction on the one store connection; the persister fell behind, memory grew tenfold and
// Close waited for the backlog without a deadline. On a slow disk, output is group-committed
// (a few transactions, not one per chunk) and Close returns within its bounded wait.
func TestSlowStoreCannotMakeCloseHangOrCommitPerChunk(t *testing.T) {
	var commits atomic.Int32
	state, err := store.Open(context.Background(), store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if point == "append_before_commit" {
			commits.Add(1)
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	manager, err := NewManager(ManagerConfig{
		Store:       state,
		Launch:      func(pty.Command) (PTYProcess, error) { return &floodProcess{done: make(chan struct{})}, nil },
		MaxSessions: 8, MaxAttachments: 2, MaxInputDecisions: 2,
		HistoryBytes: 64 << 10, AttachmentBytes: 64 << 10,
		TerminationTimeout: 2 * time.Second, TerminationGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		created, err := manager.Create(context.Background(), CreateRequest{Name: fmt.Sprintf("flood-%d", i), Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}
	time.Sleep(time.Second)
	// Three sessions print ~3000 chunks a second; a commit per chunk would be thousands.
	t.Logf("store commits in one second: %d", commits.Load())
	if got := commits.Load(); got > 15 {
		t.Fatalf("%d store commits in one second of output from three sessions; want group commits", got)
	}
	started := time.Now()
	if _, err := manager.Close(context.Background(), ids[0]); err != nil {
		t.Fatalf("close while the store is slow: %v", err)
	}
	if elapsed := time.Since(started); elapsed > outputFlushWait+3*time.Second {
		t.Fatalf("close took %s on a slow store", elapsed)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown while the store is slow: %v", err)
	}
}

// Lane W load test: after closing every flooded session the heap kept ~0.9 MiB per closed
// session, far above its 64 KiB history. A closed session keeps its listing, not its buffers.
func TestClosedSessionsReleaseTheirOutputBuffers(t *testing.T) {
	const sessions = 20
	const historyBytes = 64 << 10
	state, err := store.Open(context.Background(), store.Config{Root: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	manager, err := NewManager(ManagerConfig{
		Store:       state,
		Launch:      func(pty.Command) (PTYProcess, error) { return &floodProcess{done: make(chan struct{})}, nil },
		MaxSessions: sessions, MaxAttachments: 4, MaxInputDecisions: 4,
		HistoryBytes: historyBytes, AttachmentBytes: 1 << 20,
		TerminationTimeout: 2 * time.Second, TerminationGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	baseline := heap()
	var ids []string
	for i := 0; i < sessions; i++ {
		created, err := manager.Create(context.Background(), CreateRequest{Name: fmt.Sprintf("released-%d", i), Command: pty.Command{Path: "/bin/sh", CWD: "/tmp", Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AttachLive(created.ID, fmt.Sprintf("att_viewer_%d", i)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}
	time.Sleep(500 * time.Millisecond)
	for i, id := range ids {
		if _, err := manager.Close(context.Background(), id); err != nil {
			t.Fatalf("close: %v", err)
		}
		// A closed session keeps the last output an attached viewer is owed (9d291bdc); the viewer's
		// stream ends after terminal_stream_end and detaches, and then nothing may stay behind.
		if err := manager.Detach(id, fmt.Sprintf("att_viewer_%d", i)); err != nil {
			t.Fatalf("detach: %v", err)
		}
	}
	retained := int64(heap()) - int64(baseline)
	if limit := int64(sessions*historyBytes + 4<<20); retained > limit {
		t.Fatalf("heap kept %d KiB after closing %d flooded sessions; want at most %d KiB", retained>>10, sessions, limit>>10)
	}
}
