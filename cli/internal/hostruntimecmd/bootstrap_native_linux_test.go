//go:build linux

package hostruntimecmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

// The child listens on a port this run picked (passed in its environment), never a fixed one.
const nativeBootstrapHealthPortEnv = "PAPERBOAT_NATIVE_BOOTSTRAP_HEALTH_PORT"

func nativeBootstrapHealthURL(port string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz"
}

func nativeBootstrapHealthPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

func TestNativeBootstrapSystemdServiceProcess(t *testing.T) {
	if os.Getenv("PAPERBOAT_NATIVE_BOOTSTRAP_CHILD") != "1" {
		t.Skip("native bootstrap child only")
	}
	server := &http.Server{Addr: net.JoinHostPort("127.0.0.1", os.Getenv(nativeBootstrapHealthPortEnv)), Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func TestNativeBootstrapSystemdFailureRetryAndCleanup(t *testing.T) {
	if os.Getenv("PAPERBOAT_NATIVE_BOOTSTRAP_TEST") != "1" {
		t.Skip("set PAPERBOAT_NATIVE_BOOTSTRAP_TEST=1 on a host with user systemd")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		t.Fatal("native system service test must run as root with an isolated test account")
	}
	definition := "/etc/systemd/system/paperboat-runtime-host.service"
	if _, err := os.Lstat(definition); err == nil {
		t.Fatalf("refusing to replace existing service definition %s", definition)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	healthPort := nativeBootstrapHealthPort(t)
	runner := &failFirstRunner{next: service.ExecRunner{}}
	installer, err := service.New(service.Config{
		Platform: "linux", ConfigRoot: "/", Executable: executable, User: "nobody", Group: "nogroup",
		Arguments:   []string{"-test.run=^TestNativeBootstrapSystemdServiceProcess$", "-test.v"},
		Environment: map[string]string{"PAPERBOAT_NATIVE_BOOTSTRAP_CHILD": "1", nativeBootstrapHealthPortEnv: healthPort},
		Controller:  service.SystemdController{Runner: runner},
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
		t.Fatalf("system manager calls=%d", runner.calls)
	}
	waitForBootstrapHealth(t, healthPort, true)
	if err := installer.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	cleanupNeeded = false
	waitForBootstrapHealth(t, healthPort, false)
}

type failFirstRunner struct {
	mu    sync.Mutex
	calls int
	next  service.Runner
}

func (r *failFirstRunner) Run(ctx context.Context, name string, args ...string) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 1 {
		return errors.New("injected system manager failure")
	}
	return r.next.Run(ctx, name, args...)
}

func waitForBootstrapHealth(t *testing.T, port string, wantReady bool) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get(nativeBootstrapHealthURL(port))
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
