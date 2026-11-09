package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// doctorWithoutADaemon runs `pb args...` as on goos with no local service answering and a service start that fails.
func doctorWithoutADaemon(t *testing.T, goos string, args ...string) (int, string, string) {
	t.Helper()
	root := commandRuntimeTestRoot(t)
	home, runtimeRoot := filepath.Join(root, "home"), filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, "https://api.example.test")
	previousGOOS, previousInstaller := commandGOOS, installLocalDaemonService
	t.Cleanup(func() { commandGOOS, installLocalDaemonService = previousGOOS, previousInstaller })
	commandGOOS = goos
	installLocalDaemonService = func(context.Context, string, string, string) error {
		return errors.New("the service did not start")
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"--config", configPath}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// victus 2026-11-10: an update killed after it stopped the services left no local daemon, and plain `pb doctor`
// said only "Paperboat could not finish the command". Windows cannot restart the service from an ordinary command,
// so doctor reports it as a failing check with the repair, in the shape Linux reports a stopped host service.
func TestWindowsDoctorReportsAStoppedLocalServiceWithTheRepair(t *testing.T) {
	code, stdout, stderr := doctorWithoutADaemon(t, "windows", "doctor", "--json")
	var report doctorpkg.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || code != 1 || stderr != "" {
		t.Fatalf("code=%d decode=%v stdout=%q stderr=%q", code, err, stdout, stderr)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if report.Overall != "unhealthy" || len(report.Checks) != 1 {
		t.Fatalf("report=%+v", report)
	}
	check := report.Checks[0]
	if check.Code != "daemon" || check.Status != doctorpkg.StatusFail || !strings.Contains(check.Summary, "not running") || !strings.Contains(check.Recovery, "`pb doctor --repair`") {
		t.Fatalf("check=%+v", check)
	}
	code, stdout, stderr = doctorWithoutADaemon(t, "windows", "doctor")
	if code != 1 || !strings.Contains(stdout, "daemon: fail") || !strings.Contains(stdout, "Recovery:") || !strings.Contains(stdout, "pb doctor --repair") || strings.Contains(stdout+stderr, "could not finish") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// pb status has no report to put it in: the same sentence is its error.
func TestWindowsStatusNamesTheRepairWhenTheLocalServiceIsStopped(t *testing.T) {
	code, stdout, stderr := doctorWithoutADaemon(t, "windows", "status")
	if code == 0 || !strings.Contains(stderr, "not running") || !strings.Contains(stderr, "`pb doctor --repair`") || strings.Contains(stdout+stderr, "could not finish") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// Linux and macOS restart the service from the command; when that fails the command's own failure stands. A Windows
// service that is not installed keeps its own step.
func TestTheStoppedServiceWordingIsWindowsOnlyAndNeverHidesAMissingService(t *testing.T) {
	stopped := fmt.Errorf("read local Paperboat status: %w", errors.Join(localDaemonConnectError{socket: "pipe", cause: os.ErrNotExist}, context.DeadlineExceeded))
	if !localDaemonStopped("windows", stopped) || localDaemonStopped("linux", stopped) || localDaemonStopped("darwin", stopped) {
		t.Fatal("the stopped local service was not recognised on Windows only")
	}
	missing := errors.Join(stopped, fmt.Errorf("%w: %w", localdaemon.ErrServiceNotInstalled, os.ErrNotExist))
	if localDaemonStopped("windows", missing) {
		t.Fatal("a service that is not installed was reported as stopped")
	}
	if localDaemonStopped("windows", os.ErrNotExist) {
		t.Fatal("an unrelated missing file was reported as a stopped service")
	}
	if code, stdout, stderr := doctorWithoutADaemon(t, "linux", "doctor", "--json"); code == 0 || !strings.Contains(stdout, `"ok":false`) || strings.Contains(stdout+stderr, "pb doctor --repair") {
		t.Fatalf("linux: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
