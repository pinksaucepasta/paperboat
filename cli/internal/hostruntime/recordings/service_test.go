package recordings

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/recording"
)

func testService(t *testing.T, config Config) (*Service, *ecdh.PrivateKey) {
	t.Helper()
	if config.Root == "" {
		config.Root = filepath.Join(t.TempDir(), "recordings")
	}
	s, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Shutdown() })
	team, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return s, team
}

func start(t *testing.T, s *Service, team *ecdh.PrivateKey, sessionID string, quota int64, screen string) State {
	t.Helper()
	p, err := s.Prepare(sessionID, StartRequest{Trigger: TriggerManual, ActorAccountID: "acct_a", Accounts: []string{"acct_a"}, Key: Key{Generation: 1, Public: team.PublicKey().Bytes()}, QuotaBytes: quota})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Activate(p, 80, 24, []byte(screen))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// replay decrypts every segment of a recording with the team key, as a viewer does.
func replay(t *testing.T, s *Service, team *ecdh.PrivateKey, meta Meta) (string, recording.SegmentHeader) {
	t.Helper()
	var out strings.Builder
	var last recording.SegmentHeader
	for index := uint32(0); index < meta.Segments; index++ {
		file, err := s.ReadSegment(meta.ID, index)
		if err != nil {
			t.Fatal(err)
		}
		header, err := recording.ParseSegmentHeader(file)
		if err != nil {
			t.Fatal(err)
		}
		key, err := recording.OpenDataKey(team.Bytes(), meta.ID, header.KeyGeneration, header.SealedKey)
		if err != nil {
			t.Fatal(err)
		}
		header, plaintext, err := recording.OpenSegment(file, key)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(plaintext)
		last = header
	}
	return out.String(), last
}

func TestRecordingCapturesScreenOutputAndResizeOnly(t *testing.T) {
	s, team := testService(t, Config{SegmentBytes: 64})
	state := start(t, s, team, "sess_1", 1<<20, "$ ")
	if got := s.State("sess_1"); got == nil || got.ID != state.ID || got.Trigger != TriggerManual || got.StartedBy != "acct_a" {
		t.Fatalf("state %+v", got)
	}
	now := time.Now()
	// "é" split across two PTY reads must be recorded whole.
	s.Output("sess_1", now, []byte("caf\xc3"))
	s.Output("sess_1", now.Add(time.Second), []byte("\xa9 ls\r\n"))
	s.Resize("sess_1", now.Add(2*time.Second), 120, 40)
	s.Join("sess_1", "acct_b")
	meta, err := s.Stop("sess_1", "acct_a", ReasonStopped)
	if err != nil {
		t.Fatal(err)
	}
	if s.State("sess_1") != nil {
		t.Fatal("still recording after stop")
	}
	if meta.EndReason != ReasonStopped || meta.StoppedBy != "acct_a" || meta.Segments < 1 || strings.Join(meta.Accounts, ",") != "acct_a,acct_b" {
		t.Fatalf("meta %+v", meta)
	}
	text, last := replay(t, s, team, meta)
	if !last.Final || last.EndReason != ReasonStopped {
		t.Fatalf("last segment %+v", last)
	}
	for _, want := range []string{`{"version":2,"width":80,"height":24`, `"o","$ "`, `"o","é ls\r\n"`, `"r","120x40"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("recording lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\ufffd") {
		t.Fatal("a split character was recorded as U+FFFD")
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{s.config.Root: 0o700, s.dir(meta.ID): 0o700, filepath.Join(s.dir(meta.ID), segmentName(0)): 0o600, filepath.Join(s.dir(meta.ID), metaFile): 0o600} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s mode %v %v, want %v", path, info.Mode().Perm(), err, want)
			}
		}
	}
}

// A recorder that never drains must not slow the PTY loop: Output returns at once and drops what
// does not fit, and the recording later marks the gap.
func TestStuckRecorderNeverBlocksOutput(t *testing.T) {
	s, team := testService(t, Config{QueueBytes: 4 << 10})
	p, err := s.Prepare("sess_1", StartRequest{Trigger: TriggerRule, Key: Key{Generation: 1, Public: team.PublicKey().Bytes()}, QuotaBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	header, _ := recording.HeaderLine(recording.Header{Width: 80, Height: 24})
	r := newRecorder(s, p, header)
	s.active["sess_1"] = r // registered without its goroutine: nothing drains
	chunk := make([]byte, 1024)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			s.Output("sess_1", time.Now(), chunk)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Output blocked on a stuck recorder")
	}
	if r.dropped.Load() == 0 || r.queued.Load() > 4<<10 {
		t.Fatalf("dropped %d queued %d", r.dropped.Load(), r.queued.Load())
	}
	go r.run()
	meta, err := s.Stop("sess_1", "", ReasonStopped)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := replay(t, s, team, meta); !strings.Contains(text, `"m","gap: `) {
		t.Fatalf("no gap marker:\n%.300s", text)
	}
}

func TestQuotaDeletesOldestFinishedThenStopsActive(t *testing.T) {
	s, team := testService(t, Config{SegmentBytes: 256})
	noise := func() string {
		random := make([]byte, 1500)
		_, _ = rand.Read(random)
		return hex.EncodeToString(random) // compresses to about half: each segment is ~1.6 KB
	}
	start(t, s, team, "old", 1<<20, noise())
	old, err := s.Stop("old", "acct_a", ReasonStopped)
	if err != nil {
		t.Fatal(err)
	}
	// The new recording's segments need the old one's room, and then more than the quota holds.
	quota := old.Bytes + 1000
	start(t, s, team, "new", quota, "")
	for i := 0; i < 20 && s.State("new") != nil; i++ {
		s.Output("new", time.Now(), []byte(noise()))
		time.Sleep(20 * time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.State("new") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.State("new") != nil {
		t.Fatal("recording kept going past the quota")
	}
	metas, err := s.List()
	if err != nil || len(metas) != 1 || metas[0].EndReason != ReasonQuota || metas[0].Segments == 0 {
		t.Fatalf("metas %+v %v", metas, err)
	}
	if _, err := os.Stat(s.dir(old.ID)); !os.IsNotExist(err) {
		t.Fatalf("the oldest finished recording was not deleted for room: %v", err)
	}
}

func TestRetentionAndInterruptedRecordings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recordings")
	now := time.Now()
	clock := func() time.Time { return now }
	s, team := testService(t, Config{Root: root, Now: clock})
	start(t, s, team, "a", 1<<20, "a")
	if _, err := s.Stop("a", "", ReasonStopped); err != nil {
		t.Fatal(err)
	}
	start(t, s, team, "b", 1<<20, "b") // left open, as when the host process dies
	s.Shutdown()
	_ = s.writeMeta(Meta{ID: metaOf(t, s, "b").ID, SessionID: "b", Trigger: TriggerManual, StartedBy: "acct_a", KeyGeneration: 1, StartedAt: now})
	reopened, err := Open(Config{Root: root, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := reopened.Prune(time.Hour, 1); err != nil || len(deleted) != 1 || deleted[0].SessionID != "a" || deleted[0].EndReason != ReasonQuota {
		t.Fatalf("quota prune %+v %v", deleted, err)
	}
	closed, err := reopened.CloseInterrupted(metaOf(t, reopened, "b"))
	if err != nil || closed.EndReason != ReasonError || metaOf(t, reopened, "b").EndedAt == nil {
		t.Fatalf("closed %+v %v", closed, err)
	}
	now = now.Add(48 * time.Hour)
	deleted, err := reopened.Prune(24*time.Hour, 1<<30)
	if err != nil || len(deleted) != 1 || deleted[0].EndReason != ReasonRetention {
		t.Fatalf("deleted %+v %v", deleted, err)
	}
	if metas, _ := reopened.List(); len(metas) != 0 {
		t.Fatalf("left %+v", metas)
	}
}

func TestPrepareRefusesBadRequests(t *testing.T) {
	s, team := testService(t, Config{})
	key := Key{Generation: 1, Public: team.PublicKey().Bytes()}
	for _, request := range []StartRequest{
		{Trigger: "other", Key: key, QuotaBytes: 1},
		{Trigger: TriggerManual, Key: key, QuotaBytes: 1}, // manual needs who started it
		{Trigger: TriggerRule, Key: Key{Generation: 1, Public: []byte("short")}, QuotaBytes: 1},
		{Trigger: TriggerRule, Key: key},
	} {
		if _, err := s.Prepare("s", request); err == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
	start(t, s, team, "s", 1<<20, "")
	if _, err := s.Prepare("s", StartRequest{Trigger: TriggerRule, Key: key, QuotaBytes: 1 << 20}); err != ErrRecording {
		t.Fatalf("second start: %v", err)
	}
	if _, err := s.ReadSegment("../x", 0); err != ErrNotFound {
		t.Fatalf("path escape: %v", err)
	}
}

func metaOf(t *testing.T, s *Service, sessionID string) Meta {
	t.Helper()
	metas, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range metas {
		if meta.SessionID == sessionID {
			return meta
		}
	}
	t.Fatalf("no recording of %s", sessionID)
	return Meta{}
}

func mustList(t *testing.T, s *Service) []Meta {
	t.Helper()
	metas, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	return metas
}
