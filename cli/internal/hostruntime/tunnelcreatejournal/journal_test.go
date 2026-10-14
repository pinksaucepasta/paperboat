package tunnelcreatejournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestJournalResumesStableKeysAndStagesThenCompletes(t *testing.T) {
	config := testConfig(t)
	first, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatalf("Begin first: %v", err)
	}
	initial := first.Snapshot()
	if err := first.RecordTunnel(t.Context(), "tun_1", "operation_1"); err != nil {
		t.Fatalf("RecordTunnel: %v", err)
	}
	if err := first.RecordConnectorReady(t.Context()); err != nil {
		t.Fatalf("RecordConnectorReady: %v", err)
	}
	if err := first.RecordDomain(t.Context(), 0, "domain_1"); err != nil {
		t.Fatalf("RecordDomain: %v", err)
	}
	path := first.path
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	second, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatalf("Begin second: %v", err)
	}
	resumed := second.Snapshot()
	if resumed.TunnelKey != initial.TunnelKey || resumed.Domains[0].Key != initial.Domains[0].Key || resumed.TunnelID != "tun_1" || resumed.Domains[0].ID != "domain_1" || resumed.Stage != StageDomainsReady {
		t.Fatalf("resumed = %#v initial = %#v", resumed, initial)
	}
	if err := second.Complete(t.Context()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains after completion: %v", err)
	}
}

func TestJournalFailsClosedForMismatchCorruptionAndConcurrentUse(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		config := testConfig(t)
		session, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		config.RequestDigest = digest("different")
		if _, err := Begin(t.Context(), config); !errors.Is(err, ErrRequestMismatch) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		config := testConfig(t)
		session, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		path := session.path
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"schema":"wrong","tunnel_key":"secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Begin(t.Context(), config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("parser cause and recovery", func(t *testing.T) {
		config := testConfig(t)
		session, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		path := session.path
		before := session.Snapshot()
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		corrupt := []byte(`{"domains":"PRIVATE_JOURNAL_VALUE"}`)
		if err := os.WriteFile(path, corrupt, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = Begin(t.Context(), config)
		var decodeCause *json.UnmarshalTypeError
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &decodeCause) {
			t.Fatal("typed journal parser cause lost")
		}
		if strings.Contains(err.Error(), "PRIVATE_JOURNAL_VALUE") || strings.Contains(err.Error(), path) {
			t.Fatal("private journal data exposed")
		}
		unchanged, readErr := os.ReadFile(path)
		if readErr != nil || string(unchanged) != string(corrupt) {
			t.Fatal("corrupt journal replaced")
		}
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		recovered, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		if recovered.Snapshot().TunnelKey != before.TunnelKey {
			t.Fatal("restored journal lost stable key")
		}
		if err := recovered.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("key cause privacy and recovery", func(t *testing.T) {
		config := testConfig(t)
		healthy := config.NewKey
		spy := &journalPrivateCause{}
		config.NewKey = func() (string, error) { return "", spy }
		_, err := Begin(t.Context(), config)
		if !errors.Is(err, spy) {
			t.Fatal("key failure cause lost")
		}
		_ = err.Error()
		config.NewKey = healthy
		session, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("filesystem privacy and recovery", func(t *testing.T) {
		config := testConfig(t)
		config.StateRoot = filepath.Join(config.StateRoot, "private-invalid-root")
		if err := os.WriteFile(config.StateRoot, []byte("private data"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Begin(t.Context(), config)
		var native *os.PathError
		if !errors.As(err, &native) {
			t.Fatal("original filesystem cause lost")
		}
		if strings.Contains(err.Error(), config.StateRoot) {
			t.Fatal("private filesystem path exposed")
		}
		if err := os.Remove(config.StateRoot); err != nil {
			t.Fatal(err)
		}
		session, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("locked", func(t *testing.T) {
		config := testConfig(t)
		first, err := Begin(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		if _, err := Begin(t.Context(), config); !errors.Is(err, ErrLocked) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestJournalRejectsCancellationPermissionsAndSymlink(t *testing.T) {
	config := testConfig(t)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := Begin(expired, config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired deadline was replaced by cancellation")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Begin(ctx, config); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	session, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	path := session.path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(t.Context(), config); !errors.Is(err, ErrInvalid) {
		t.Fatalf("permission error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(t.Context(), config); err == nil {
		t.Fatal("symlink journal was accepted")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "keep" {
		t.Fatalf("target changed: %q %v", content, err)
	}
}

func TestJournalConcurrentSnapshotsAreCopies(t *testing.T) {
	config := testConfig(t)
	session, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			snapshot := session.Snapshot()
			snapshot.Domains[0].Key = "modified"
		}
	}()
	for range 100 {
		if !strings.HasPrefix(session.Snapshot().Domains[0].Key, "key_") {
			t.Fatal("snapshot mutation escaped")
		}
	}
	<-done
}

func TestJournalEnforcesWorkflowOrderAndPartialDomainProgress(t *testing.T) {
	config := testConfig(t)
	config.DomainCount = 2
	config.RequestDigest = digest("two-domains")
	session, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RecordDomain(t.Context(), 0, "domain_1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RecordDomain before connector ready = %v", err)
	}
	if got := session.Snapshot(); got.Stage != StagePrepared || got.Domains[0].ID != "" {
		t.Fatalf("failed early domain write changed journal: %#v", got)
	}
	if err := session.RecordTunnel(t.Context(), "tun_1", "operation_1"); err != nil {
		t.Fatal(err)
	}
	if err := session.RecordDomain(t.Context(), 0, "domain_1"); err != nil {
		t.Fatalf("RecordDomain before connector ready: %v", err)
	}
	partial := session.Snapshot()
	if partial.Stage != StageTunnelCreated || partial.Domains[0].ID != "domain_1" || partial.Domains[1].ID != "" {
		t.Fatalf("pre-connector domain journal = %#v", partial)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	session, err = Begin(t.Context(), config)
	if err != nil {
		t.Fatalf("resume before connector ready: %v", err)
	}
	if resumed := session.Snapshot(); resumed.Stage != StageTunnelCreated || resumed.Domains[0].ID != "domain_1" {
		t.Fatalf("resumed pre-connector journal = %#v", resumed)
	}
	if err := session.RecordConnectorReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if afterConnector := session.Snapshot(); afterConnector.Stage != StageConnectorReady || afterConnector.Domains[0].ID != "domain_1" || afterConnector.Domains[1].ID != "" {
		t.Fatalf("post-connector partial domain journal = %#v", afterConnector)
	}
	if err := session.RecordDomain(t.Context(), 1, "domain_2"); err != nil {
		t.Fatal(err)
	}
	if got := session.Snapshot(); got.Stage != StageDomainsReady {
		t.Fatalf("complete domain journal stage = %q", got.Stage)
	}
	if err := session.Complete(t.Context()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := session.Complete(t.Context()); err != nil {
		t.Fatalf("idempotent Complete: %v", err)
	}
	if err := session.RecordTunnel(t.Context(), "tun_1", "operation_1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RecordTunnel after Complete = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatalf("Begin after completed journal: %v", err)
	}
	if got := resumed.Snapshot(); got.Stage != StagePrepared || got.TunnelID != "" || len(got.Domains) != 2 {
		t.Fatalf("new journal after completion = %#v", got)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalCompletesTunnelWithoutDomains(t *testing.T) {
	config := testConfig(t)
	config.DomainCount = 0
	config.RequestDigest = digest("no-domains")
	session, err := Begin(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RecordTunnel(t.Context(), "tun_1", "operation_1"); err != nil {
		t.Fatal(err)
	}
	if err := session.Complete(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Complete before connector ready = %v", err)
	}
	if err := session.RecordConnectorReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.Complete(t.Context()); err != nil {
		t.Fatalf("Complete without domains: %v", err)
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	var sequence atomic.Int64
	expires := time.Unix(200, 0).UTC()
	return Config{
		StateRoot:     t.TempDir(),
		HostID:        "host_1",
		NameDigest:    digest("demo"),
		RequestDigest: digest("demo|public|http|127.0.0.1:8080|domain"),
		DomainCount:   1,
		ExpiresAt:     &expires,
		NewKey: func() (string, error) {
			return "key_" + strings.Repeat("x", int(sequence.Add(1))), nil
		},
	}
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type journalPrivateCause struct{}

func (*journalPrivateCause) Error() string { panic("private journal cause formatted") }
