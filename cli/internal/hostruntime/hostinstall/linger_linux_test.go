//go:build linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type lingerFixture struct {
	paths installPaths
	calls [][]string
}

// newLingerFixture points systemd's linger directory, runtime marker and loginctl at a temporary
// tree. The fake loginctl mutates the directory like logind does.
func newLingerFixture(t *testing.T) *lingerFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &lingerFixture{paths: installPaths{runtimeState: filepath.Join(root, "state")}}
	oldDir, oldRuntime, oldRun, oldOwner := lingerDirectory, systemdRuntime, runLoginctl, stateOwner
	lingerDirectory, systemdRuntime = filepath.Join(root, "linger"), filepath.Join(root, "systemd")
	stateOwner = func() int { return os.Geteuid() }
	for _, directory := range []string{lingerDirectory, systemdRuntime} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runLoginctl = func(_ context.Context, arguments ...string) error {
		fixture.calls = append(fixture.calls, arguments)
		path := filepath.Join(lingerDirectory, arguments[1])
		if arguments[0] == "enable-linger" {
			return os.WriteFile(path, nil, 0o644)
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { lingerDirectory, systemdRuntime, runLoginctl, stateOwner = oldDir, oldRuntime, oldRun, oldOwner })
	return fixture
}

var lingerRequest = Request{User: "tester", UID: 1001}

func TestInstallEnablesLingerAndUninstallRestoresIt(t *testing.T) {
	fixture := newLingerFixture(t)
	if err := ensureUserLinger(context.Background(), lingerRequest, fixture.paths); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := userLingers("tester"); !enabled {
		t.Fatal("install left the user without linger: the daemon would stop at logout")
	}
	// A second install must not forget that Paperboat enabled it.
	if err := ensureUserLinger(context.Background(), lingerRequest, fixture.paths); err != nil {
		t.Fatal(err)
	}
	if err := restoreUserLinger(context.Background(), lingerRequest, fixture.paths); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := userLingers("tester"); enabled {
		t.Fatal("uninstall did not restore the logout setting it changed")
	}
	if _, err := os.Stat(filepath.Join(fixture.paths.runtimeState, lingerBaselineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore left its baseline behind: %v", err)
	}
	want := [][]string{{"enable-linger", "tester"}, {"disable-linger", "tester"}}
	if !reflect.DeepEqual(fixture.calls, want) {
		t.Fatalf("loginctl calls = %v, want %v", fixture.calls, want)
	}
}

func TestAnAdministratorsLingerSettingIsLeftAlone(t *testing.T) {
	fixture := newLingerFixture(t)
	if err := os.WriteFile(filepath.Join(lingerDirectory, "tester"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureUserLinger(context.Background(), lingerRequest, fixture.paths); err != nil {
		t.Fatal(err)
	}
	if err := restoreUserLinger(context.Background(), lingerRequest, fixture.paths); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := userLingers("tester"); !enabled || len(fixture.calls) != 0 {
		t.Fatalf("Paperboat changed a setting it did not make: enabled=%v calls=%v", enabled, fixture.calls)
	}
}

func TestLingerFailureIsReportedAndLeavesNoBaseline(t *testing.T) {
	fixture := newLingerFixture(t)
	runLoginctl = func(context.Context, ...string) error { return errors.New("denied") }
	if err := ensureUserLinger(context.Background(), lingerRequest, fixture.paths); err == nil {
		t.Fatal("install continued although the daemon would stop at logout")
	}
	if _, err := os.Stat(filepath.Join(fixture.paths.runtimeState, lingerBaselineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed enable kept a baseline: %v", err)
	}
}

func TestLingerIsSkippedWithoutSystemdAndForRoot(t *testing.T) {
	fixture := newLingerFixture(t)
	if err := os.Remove(systemdRuntime); err != nil {
		t.Fatal(err)
	}
	if err := ensureUserLinger(context.Background(), lingerRequest, fixture.paths); err != nil || len(fixture.calls) != 0 {
		t.Fatalf("a host without systemd was touched: %v %v", err, fixture.calls)
	}
	if err := os.MkdirAll(systemdRuntime, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureUserLinger(context.Background(), Request{User: "root", UID: 0}, fixture.paths); err != nil || len(fixture.calls) != 0 {
		t.Fatalf("the root account was given linger: %v %v", err, fixture.calls)
	}
}

func TestLingerRejectsUnsafeUserNames(t *testing.T) {
	fixture := newLingerFixture(t)
	for _, name := range []string{"", "../etc", "a/b", "UPPER", "bad name"} {
		if err := ensureUserLinger(context.Background(), Request{User: name, UID: 1001}, fixture.paths); err == nil {
			t.Fatalf("user name %q was accepted", name)
		}
	}
	if len(fixture.calls) != 0 {
		t.Fatalf("loginctl ran for an unsafe name: %v", fixture.calls)
	}
}
