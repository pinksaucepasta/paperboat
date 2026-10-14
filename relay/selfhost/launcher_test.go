package selfhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type shellRuntimeLauncher struct{ script string }

func (l shellRuntimeLauncher) Validate(context.Context, string, string) error {
	_, err := os.Stat(l.script)
	return err
}
func (l shellRuntimeLauncher) Run(ctx context.Context, capability, dir string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", l.script, capability, dir)
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 6 * time.Second
	return cmd.Run()
}

func TestSupervisorRuntimeLauncherWithoutLocalBinaries(t *testing.T) {
	dir, _, _, server := testInstallation(t, false)
	server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	state, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	state.Config.Listen = address
	claim := testClaim(false)
	state.Claim = &claim
	if err := applyClaim(t.Context(), dir, &state); err != nil {
		t.Fatal(err)
	}
	// Reproduce the binary-only prerequisite: enrollment is ready but there are
	// no installed component binaries. A launcher must be able to own the runtime.
	err = Run(t.Context(), dir, t.TempDir(), io.Discard, nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binary prerequisite: %v", err)
	}
	script := filepath.Join(t.TempDir(), "container-lifecycle.sh")
	if err := os.WriteFile(script, []byte(`echo $$ > "$2/launcher.pid"
sleep 60 &
child=$!
trap 'kill "$child" 2>/dev/null || true; wait; exit 0' TERM INT
wait "$child"
`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	var output bytes.Buffer
	go func() { done <- RunWithLauncher(ctx, dir, shellRuntimeLauncher{script}, &output, nil) }()
	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for pid == 0 && time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, "relay", "launcher.pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("injected runtime did not drain")
	}
	if pid == 0 {
		t.Fatal("injected runtime did not launch without local binaries")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("injected runtime survived installation shutdown")
	}
}
