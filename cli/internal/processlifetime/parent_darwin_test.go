//go:build darwin

package processlifetime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"golang.org/x/sys/unix"
)

const (
	parentWatchFailureRoleEnv = "PAPERBOAT_TEST_PARENT_WATCH_FAILURE_ROLE"
	parentWatchFailureFileEnv = "PAPERBOAT_TEST_PARENT_WATCH_FAILURE_FILE"
	parentWatchReference      = "support_00000000-0000-4000-8000-000000000001"
)

func TestParentWatchFailureIsObservedAndTerminatesChild(t *testing.T) {
	if os.Getenv(parentWatchFailureRoleEnv) == "child" {
		path := os.Getenv(parentWatchFailureFileEnv)
		restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
			record := struct {
				Component, Operation, Stage, Code, SupportReference string
				Outcome                                             string
			}{fault.Component, fault.Operation, fault.Stage, fault.Code, fault.SupportReference, fault.Outcome}
			encoded, err := json.Marshal(record)
			if err != nil {
				panic(err)
			}
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				panic(err)
			}
		})
		defer restore()

		ctx, cancel := context.WithCancel(supportref.WithContext(context.Background(), parentWatchReference))
		cancel()
		queue, err := unix.Kqueue()
		if err != nil {
			panic(err)
		}
		if err := unix.Close(queue); err != nil {
			panic(err)
		}
		startParentWatch(ctx, queue, unix.Getppid())
		select {}
	}

	reportPath := filepath.Join(t.TempDir(), "observed-failure.json")
	command := exec.Command(os.Args[0], "-test.run=^TestParentWatchFailureIsObservedAndTerminatesChild$")
	command.Env = append(os.Environ(), parentWatchFailureRoleEnv+"=child", parentWatchFailureFileEnv+"="+reportPath)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("watcher child did not terminate: err=%v output=%s", err, output)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("watcher child was not terminated by SIGTERM: status=%v output=%s", exitErr.Sys(), output)
	}

	encoded, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("watcher fault was not recorded before termination: %v", err)
	}
	var observed struct {
		Component, Operation, Stage, Code, SupportReference string
		Outcome                                             string
	}
	if err := json.Unmarshal(encoded, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Component != "paperboat-cli" || observed.Operation != "ssh" || observed.Stage != "lifecycle" || observed.Code != "managed_ssh_failed" || observed.Outcome != "failed" || observed.SupportReference != parentWatchReference {
		t.Fatalf("watcher fault lost its safe classification or invocation reference: %+v", observed)
	}
	if !supportref.Valid(observed.SupportReference) {
		t.Fatalf("watcher fault has invalid support reference: %q", observed.SupportReference)
	}
}

func TestParentWatchSetupCloseFailurePreservesBothCauses(t *testing.T) {
	queue, err := unix.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(queue); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("registration failed")
	got := closeQueueAfterSetupFailure(queue, cause)
	if !errors.Is(got, cause) || !errors.Is(got, unix.EBADF) {
		t.Fatalf("setup/close failure lost a cause: %v", got)
	}
}
