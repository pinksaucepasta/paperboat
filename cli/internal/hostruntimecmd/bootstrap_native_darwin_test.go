//go:build darwin

package hostruntimecmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

// The child listens on a port this run picked (passed in its environment), never a fixed one.
const nativeBootstrapDarwinHealthPortEnv = "PAPERBOAT_NATIVE_BOOTSTRAP_DARWIN_HEALTH_PORT"

func nativeBootstrapDarwinHealthURL(port string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz"
}

func nativeBootstrapDarwinHealthPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

func TestNativeBootstrapLaunchdServiceProcess(t *testing.T) {
	if os.Getenv("PAPERBOAT_NATIVE_BOOTSTRAP_DARWIN_CHILD") != "1" {
		t.Skip("native bootstrap child only")
	}
	server := &http.Server{Addr: net.JoinHostPort("127.0.0.1", os.Getenv(nativeBootstrapDarwinHealthPortEnv)), Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func TestNativeBootstrapLaunchdFailureRetryAndCleanup(t *testing.T) {
	if os.Getenv("PAPERBOAT_NATIVE_BOOTSTRAP_DARWIN_TEST") != "1" {
		t.Skip("set PAPERBOAT_NATIVE_BOOTSTRAP_DARWIN_TEST=1 in a logged-in macOS user session")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		t.Fatal("native LaunchDaemon test must run as root with an isolated test account")
	}
	definition := filepath.Join("/Library", "LaunchDaemons", service.Label+".plist")
	if _, err := os.Lstat(definition); err == nil {
		t.Fatalf("refusing to replace existing service definition %s", definition)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	healthPort := nativeBootstrapDarwinHealthPort(t)
	runner := &darwinFailFirstRunner{next: service.ExecRunner{}}
	installer, err := service.New(service.Config{
		Platform: "darwin", ConfigRoot: "/", Executable: executable, User: "nobody", Group: "nobody",
		Arguments:   []string{"-test.run=^TestNativeBootstrapLaunchdServiceProcess$", "-test.v"},
		Environment: map[string]string{"PAPERBOAT_NATIVE_BOOTSTRAP_DARWIN_CHILD": "1", nativeBootstrapDarwinHealthPortEnv: healthPort},
		Controller:  service.LaunchdController{Runner: runner, UID: os.Getuid()},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupNeeded := true
	t.Cleanup(func() {
		if cleanupNeeded {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			_ = installer.Uninstall(ctx)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// The injected first manager call fails; one retry must converge.
	if err := installer.Install(ctx); err != nil {
		if err := installer.Install(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if runner.calls < 2 {
		t.Fatalf("launchd manager calls=%d", runner.calls)
	}
	waitForBootstrapDarwinHealth(t, healthPort, true)
	if err := installer.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	cleanupNeeded = false
	waitForBootstrapDarwinHealth(t, healthPort, false)
}

type darwinFailFirstRunner struct {
	mu    sync.Mutex
	calls int
	next  service.Runner
}

func (r *darwinFailFirstRunner) Run(ctx context.Context, name string, args ...string) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 1 {
		return errors.New("injected launchd manager failure")
	}
	return r.next.Run(ctx, name, args...)
}

func waitForBootstrapDarwinHealth(t *testing.T, port string, wantReady bool) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get(nativeBootstrapDarwinHealthURL(port))
		ready := err == nil && response.StatusCode == http.StatusOK
		if response != nil {
			response.Body.Close()
		}
		if ready == wantReady {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health ready=%v want=%v err=%v", ready, wantReady, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
