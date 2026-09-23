//go:build darwin || linux

package updated

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestHTTPHealthRejectsCandidateWithoutFreshHeartbeat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"live":true}`)) }))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// httptest uses a loopback IP but may not preserve it in URL on all hosts.
	endpoint := strings.Replace(server.URL, parsed.Hostname(), "127.0.0.1", 1) + "/healthz"
	health := HTTPHealth{Endpoint: endpoint}
	stale := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime", APIVersion: 1, Epoch: 1, LastHeartbeatUnixMilli: time.Now().Add(-16 * time.Second).UnixMilli()}
	if err := health.Check(context.Background(), stale, workerupdate.Release{}); err == nil {
		t.Fatal("stale worker heartbeat passed health")
	}
	fresh := stale
	fresh.LastHeartbeatUnixMilli = time.Now().UnixMilli()
	if err := health.Check(context.Background(), fresh, workerupdate.Release{}); err != nil {
		t.Fatalf("fresh heartbeat health=%v", err)
	}
}

func TestValidUnixWorkerIdentitySupportsOnlyExactPairs(t *testing.T) {
	for _, test := range []struct {
		uid, gid int
		want     bool
	}{
		{uid: 1000, gid: 1000, want: true},
		{uid: 0, gid: 0, want: true},
		{uid: 0, gid: 1000},
		{uid: 1000, gid: 0},
		{uid: -1, gid: -1},
	} {
		if got := validUnixWorkerIdentity(test.uid, test.gid); got != test.want {
			t.Fatalf("validUnixWorkerIdentity(%d, %d)=%v want %v", test.uid, test.gid, got, test.want)
		}
	}
}

func TestSeedUnixBlockedUpdateSurvivesHelperRetirement(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected updater state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(5 * time.Minute)
	journal := updateflow.Journal{
		Schema: updateflow.SchemaV1, TransactionID: "txn_busy", Stage: updateflow.StageIdle,
		ActiveVersion: "2026.08.27.46", ActiveDigest: strings.Repeat("a", 64), ActiveLength: 1,
		ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1,
		BootID: "hostd", StageUpdatedAt: time.Now().UTC(), LastFailure: updateflow.FailureDrain,
		BlockedReason: autoupdate.BlockedActiveTerminalSessions, RequiredVersion: "2026.08.27.47", NextCheckAt: next,
	}
	if err := updateflow.Write(filepath.Join(root, "transaction.json"), journal, 0, 0); err != nil {
		t.Fatal(err)
	}
	scheduler, err := autoupdate.New(autoupdate.Config{Check: func(context.Context) (autoupdate.Result, error) { return autoupdate.Result{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = seedUnixBlockedUpdate(root, scheduler); err != nil {
		t.Fatal(err)
	}
	state := scheduler.Snapshot()
	if state.BlockedReason != autoupdate.BlockedActiveTerminalSessions || state.RequiredVersion != journal.RequiredVersion || !state.NextCheckAt.Equal(next) {
		t.Fatalf("scheduler state=%+v", state)
	}
}

func TestResolveReleaseDoesNotActivateOrWaitForMonitor(t *testing.T) {
	called := false
	result, err := resolveRelease(context.Background(), "2026.08.27.46", func(context.Context) (workerupdate.Release, bool, error) {
		called = true
		return workerupdate.Release{Version: "2026.08.27.47"}, true, nil
	})
	if err != nil {
		t.Fatalf("resolveRelease error = %v", err)
	}
	if !called {
		t.Fatal("resolver was not called")
	}
	if result.Version != "2026.08.27.47" || result.Updated {
		t.Fatalf("result = %+v, want version-only result", result)
	}
}

func TestResolveReleaseKeepsActiveVersionWhenNoRelease(t *testing.T) {
	result, err := resolveRelease(context.Background(), "2026.08.27.46", func(context.Context) (workerupdate.Release, bool, error) {
		return workerupdate.Release{}, false, nil
	})
	if err != nil {
		t.Fatalf("resolveRelease error = %v", err)
	}
	if result.Version != "2026.08.27.46" || result.Updated {
		t.Fatalf("result = %+v, want active version", result)
	}
}
