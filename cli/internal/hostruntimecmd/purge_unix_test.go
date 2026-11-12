//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// purgeFake stands in for systemctl, launchctl, the process table, signals and
// the native uninstall and guard steps. Processes that exit on SIGTERM are
// removed from the table then; immortal ones survive SIGKILL.
type purgeFake struct {
	commands []string
	signals  []string
	alive    map[int]unixProcess
	// jobs is the service manager's view: main pid -> unit or job label.
	jobs         map[int]string
	termExits    map[int]bool
	immortal     map[int]bool
	failCommands map[string]error
	uninstallErr error
	uninstalls   int
	guards       int
}

func newPurgeFake(processes ...unixProcess) *purgeFake {
	fake := &purgeFake{alive: map[int]unixProcess{}, jobs: map[int]string{}, termExits: map[int]bool{}, immortal: map[int]bool{}, failCommands: map[string]error{}}
	for _, process := range processes {
		fake.alive[process.pid] = process
	}
	return fake
}

func (f *purgeFake) run(_ context.Context, name string, arguments ...string) error {
	command := strings.Join(append([]string{name}, arguments...), " ")
	f.commands = append(f.commands, command)
	return f.failCommands[command]
}

func (f *purgeFake) list(context.Context) ([]unixProcess, error) {
	processes := make([]unixProcess, 0, len(f.alive))
	for _, process := range f.alive {
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].pid < processes[j].pid })
	return processes, nil
}

func (f *purgeFake) listJobs(context.Context) (map[int]string, error) {
	jobs := map[int]string{}
	for pid, unit := range f.jobs {
		if _, alive := f.alive[pid]; alive {
			jobs[pid] = unit
		}
	}
	return jobs, nil
}

func (f *purgeFake) signal(pid int, sig syscall.Signal) error {
	f.signals = append(f.signals, fmt.Sprintf("%s %d", sig, pid))
	switch sig {
	case syscall.SIGTERM:
		if f.termExits[pid] {
			delete(f.alive, pid)
		}
	case syscall.SIGKILL:
		if !f.immortal[pid] {
			delete(f.alive, pid)
		}
	}
	return nil
}

func (f *purgeFake) uninstall(context.Context) error {
	f.uninstalls++
	return f.uninstallErr
}

func (f *purgeFake) guard(context.Context) error {
	f.guards++
	return nil
}

func plantFile(t *testing.T, root, path string) {
	t.Helper()
	// This root models protected host ancestry regardless of the test runner's
	// group-writable temporary-directory umask.
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func plantDir(t *testing.T, root, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
		t.Fatal(err)
	}
}

func plantLink(t *testing.T, root, path, target string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

func assertGone(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists (err=%v)", path, err)
		}
	}
}

func assertPresent(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(root, path)); err != nil {
			t.Fatalf("%s was removed but belongs to another account: %v", path, err)
		}
	}
}

func testSweep(root string, plan unixPurgePlan, fake *purgeFake) unixSweep {
	return unixSweep{
		root:          root,
		plan:          plan,
		run:           fake.run,
		remove:        os.RemoveAll,
		processes:     fake.list,
		jobs:          fake.listJobs,
		signal:        fake.signal,
		uninstall:     fake.uninstall,
		guard:         fake.guard,
		terminateWait: 0,
		killWait:      0,
	}
}

// errorChainText renders every cause reachable through Unwrap, so tests can
// assert on diagnostics that are deliberately not part of the authored message.
func errorChainText(err error) string {
	if err == nil {
		return ""
	}
	parts := []string{err.Error()}
	var children []error
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children = wrapped.Unwrap()
	case interface{ Unwrap() error }:
		if child := wrapped.Unwrap(); child != nil {
			children = []error{child}
		}
	}
	for _, child := range children {
		parts = append(parts, errorChainText(child))
	}
	return strings.Join(parts, "\n")
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}

// plantLinuxAccount creates the units and payload of one enrolled account
// and of a neighbour whose uid shares a prefix (u10010 under u1001).
func plantLinuxAccount(t *testing.T, root string) {
	t.Helper()
	systemd := "etc/systemd/system/"
	for _, unit := range []string{
		"paperboat-runtime-privileged-u1001.service",
		"paperboat-hostd-u1001.service",
		"paperboat-updated-u1001.service",
		"paperboat-session-owner-u1001-g1.service",
		"paperboat-session-owner-u1001-g2.service",
		"paperboat-carrier-u1001.service",
		"paperboat-update-activate-u1001.service",
		"paperboat-hostd-u10010.service",
		"paperboat-hostd-u1002.service",
	} {
		plantFile(t, root, systemd+unit)
	}
	plantFile(t, root, systemd+"paperboat-hostd-u1001.service.d/override.conf")
	plantLink(t, root, systemd+"multi-user.target.wants/paperboat-carrier-u1001.service", "../paperboat-carrier-u1001.service")
	for _, path := range []string{
		"usr/local/libexec/paperboat/users/u1001/bin/pb",
		"var/lib/paperboat-installer/users/u1001/install-metadata.json",
		"var/lib/paperboat/users/u1001/hostd.token",
		"var/lib/paperboat-updated-u1001/state.json",
		"var/run/paperboat-hostd-u1001/control.sock",
		"run/paperboat-carrier-u1001/socket",
		"var/cache/paperboat-u1001-cache/blob",
		"usr/local/libexec/paperboat/users/u10010/bin/pb",
		"var/lib/paperboat/users/u10010/hostd.token",
	} {
		plantFile(t, root, path)
	}
}

func TestLinuxSweepRemovesEveryGenerationOfOneAccount(t *testing.T) {
	root := t.TempDir()
	plantLinuxAccount(t, root)
	// Retired generation definitions may be gone while systemd StateDirectory remains.
	const retired = "var/lib/paperboat-session-owner-u1001-8f704253/state.json"
	const neighbour = "var/lib/paperboat-session-owner-u10010-8f704253/state.json"
	plantFile(t, root, retired)
	plantFile(t, root, neighbour)
	plantFile(t, root, "var/lib/paperboat-u1001/state.json")
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	// Another account is still installed, so the guard must stay.
	plantFile(t, root, "var/lib/paperboat-installer/users/u1002/install-metadata.json")

	// The test process and its parent stand in for the purge's own process
	// tree (the user-side pb that launched sudo is an ancestor).
	selfParent := 4000
	fake := newPurgeFake(
		unixProcess{pid: 4001, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/pb"},
		unixProcess{pid: 4002, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/hostd"},
		unixProcess{pid: 4003, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1002/bin/hostd"},
		unixProcess{pid: selfParent, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/pb"},
		unixProcess{pid: os.Getpid(), ppid: selfParent, exe: "/usr/local/libexec/paperboat/users/u1001/bin/pb"},
	)
	// The service manager's main processes; the neighbour's unit and this
	// process tree's are not this purge's to stop.
	fake.jobs = map[int]string{4001: "paperboat-hostd-u1001.service", 4002: "paperboat-carrier-u1001.service",
		4003: "paperboat-hostd-u1002.service", selfParent: "paperboat-updated-u1001.service"}
	fake.termExits[4001] = true
	fake.failCommands["/usr/bin/systemctl stop paperboat-session-owner-u1001-g2.service"] = errors.New("not loaded")
	sweep := testSweep(root, plan, fake)

	if err := sweep.purgeAccount(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, unit := range []string{
		"paperboat-runtime-privileged-u1001.service",
		"paperboat-hostd-u1001.service",
		"paperboat-updated-u1001.service",
		"paperboat-session-owner-u1001-g1.service",
		"paperboat-session-owner-u1001-g2.service",
		"paperboat-carrier-u1001.service",
		"paperboat-update-activate-u1001.service",
	} {
		for _, action := range []string{"stop", "disable"} {
			if !containsCommand(fake.commands, "/usr/bin/systemctl "+action+" "+unit) {
				t.Fatalf("missing %s for %s in %v", action, unit, fake.commands)
			}
		}
		if !strings.Contains(strings.Join(fake.commands, "\n"), "reset-failed") {
			t.Fatalf("reset-failed not run: %v", fake.commands)
		}
	}
	if !containsCommand(fake.commands, "/usr/bin/systemctl daemon-reload") {
		t.Fatalf("daemon-reload not run: %v", fake.commands)
	}
	assertGone(t, root, retired, "var/lib/paperboat-u1001",
		"etc/systemd/system/paperboat-runtime-privileged-u1001.service",
		"etc/systemd/system/paperboat-hostd-u1001.service",
		"etc/systemd/system/paperboat-hostd-u1001.service.d",
		"etc/systemd/system/paperboat-session-owner-u1001-g1.service",
		"etc/systemd/system/paperboat-session-owner-u1001-g2.service",
		"etc/systemd/system/paperboat-carrier-u1001.service",
		"etc/systemd/system/multi-user.target.wants/paperboat-carrier-u1001.service",
		"etc/systemd/system/paperboat-update-activate-u1001.service",
		"usr/local/libexec/paperboat/users/u1001",
		"var/lib/paperboat-installer/users/u1001",
		"var/lib/paperboat/users/u1001",
		"var/lib/paperboat-updated-u1001",
		"var/run/paperboat-hostd-u1001",
		"run/paperboat-carrier-u1001",
		"var/cache/paperboat-u1001-cache",
	)
	// The neighbours u10010 and u1002 must not be claimed by u1001's globs.
	assertPresent(t, root, neighbour,
		"etc/systemd/system/paperboat-hostd-u10010.service",
		"etc/systemd/system/paperboat-hostd-u1002.service",
		"usr/local/libexec/paperboat/users/u10010/bin/pb",
		"var/lib/paperboat/users/u10010/hostd.token",
		"var/lib/paperboat-installer/users/u1002/install-metadata.json",
	)

	// Processes from this account's payload are stopped; the neighbour, this
	// process and its ancestor are left alone.
	wantSignals := []string{"terminated 4001", "terminated 4002", "killed 4002"}
	if !reflect.DeepEqual(fake.signals, wantSignals) {
		t.Fatalf("signals=%v want=%v", fake.signals, wantSignals)
	}
	if fake.uninstalls != 1 {
		t.Fatalf("native uninstall calls=%d want 1 (metadata present)", fake.uninstalls)
	}
	if fake.guards != 0 {
		t.Fatalf("machine guard purged while another account remains installed")
	}
}

func TestPurgeRunsMachineGuardOnlyWhenNoAccountRemains(t *testing.T) {
	root := t.TempDir()
	plantLinuxAccount(t, root)
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	fake := newPurgeFake()
	if err := testSweep(root, plan, fake).purgeAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.guards != 1 {
		t.Fatalf("machine guard purge calls=%d want 1 when no account remains", fake.guards)
	}
}

func TestPurgeNamesEveryLeftoverAndKeepsGuardOnFailure(t *testing.T) {
	root := t.TempDir()
	plantLinuxAccount(t, root)
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	fake := newPurgeFake(unixProcess{pid: 4005, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/session-owner"})
	fake.jobs[4005] = "paperboat-session-owner-u1001-g2.service"
	fake.immortal[4005] = true
	sweep := testSweep(root, plan, fake)
	const protected = "/etc/systemd/system/paperboat-session-owner-u1001-g2.service"
	sweep.remove = func(path string) error {
		if path == filepath.Join(root, protected) {
			return nil
		}
		return os.RemoveAll(path)
	}

	err = sweep.purgeAccount(context.Background())
	if err == nil {
		t.Fatal("purge reported success with leftovers")
	}
	// The authored message stays fixed; leftover names live in the cause chain.
	message := errorChainText(err)
	for _, want := range []string{protected, "process 4005"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not name leftover %q", message, want)
		}
	}
	if strings.Contains(message, "paperboat-hostd-u1001.service") {
		t.Fatalf("error names a removed item: %q", message)
	}
	if got := err.Error(); got != "removing the Paperboat installation failed" {
		t.Fatalf("authored message = %q", got)
	}
	if fake.guards != 1 {
		t.Fatalf("guard calls=%d; the guard decision must not depend on leftovers", fake.guards)
	}
}

func TestPurgeContinuesAfterNativeUninstallFailure(t *testing.T) {
	root := t.TempDir()
	plantLinuxAccount(t, root)
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	fake := newPurgeFake()
	fake.uninstallErr = errors.New("native teardown refused")
	err = testSweep(root, plan, fake).purgeAccount(context.Background())
	var stage interface{ DiagnosticStage() string }
	if !errors.Is(err, fake.uninstallErr) || !errors.As(err, &stage) || stage.DiagnosticStage() != "native_uninstall" {
		t.Fatalf("error = %v, want native uninstall failure", err)
	}
	assertGone(t, root, "etc/systemd/system/paperboat-hostd-u1001.service", "var/lib/paperboat/users/u1001")
}

func TestDarwinSweepRemovesEveryGenerationOfOneAccount(t *testing.T) {
	root := t.TempDir()
	daemons := "Library/LaunchDaemons/"
	for _, name := range []string{
		"com.pinksaucepasta.paperboat.runtime-privileged.u501.plist",
		"com.pinksaucepasta.paperboat.hostd.u501.plist",
		"com.pinksaucepasta.paperboat.updated.u501.plist",
		"com.pinksaucepasta.paperboat.session-owner.u501.g1.plist",
		"com.pinksaucepasta.paperboat.carrier.u501.plist",
		"com.pinksaucepasta.paperboat.update-activate.u501.plist",
		"com.pinksaucepasta.paperboat.hostd.u5010.plist",
		"io.paperboat.machineguard.plist",
	} {
		plantFile(t, root, daemons+name)
	}
	plantFile(t, root, "Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb")
	plantFile(t, root, "Library/Application Support/Paperboat/users/u501/install-metadata.json")
	plantFile(t, root, "Library/Application Support/Paperboat/updated-u501/state.json")
	plantFile(t, root, "Library/Application Support/Paperboat/ipc/u501-control.sock")
	plantFile(t, root, "Library/Application Support/Paperboat/ipc/u5010-control.sock")
	plantFile(t, root, "private/var/log/com.pinksaucepasta.paperboat.hostd.u501.log")
	plantFile(t, root, "private/var/log/com.pinksaucepasta.paperboat.hostd.u5010.log")
	plan, err := newUnixPurgePlan("darwin", 501)
	if err != nil {
		t.Fatal(err)
	}
	fake := newPurgeFake()
	if err := testSweep(root, plan, fake).purgeAccount(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, label := range []string{
		service.HostLabel + ".u501",
		service.HostdLabel + ".u501",
		service.UpdaterLabel + ".u501",
		"com.pinksaucepasta.paperboat.session-owner.u501.g1",
		"com.pinksaucepasta.paperboat.carrier.u501",
		"com.pinksaucepasta.paperboat.update-activate.u501",
	} {
		if !containsCommand(fake.commands, "/bin/launchctl bootout system/"+label) {
			t.Fatalf("bootout for %s missing: %v", label, fake.commands)
		}
		assertGone(t, root, daemons+label+".plist")
	}
	assertGone(t, root,
		"Library/PrivilegedHelperTools/Paperboat/users/u501",
		"Library/Application Support/Paperboat/users/u501",
		"Library/Application Support/Paperboat/updated-u501",
		"Library/Application Support/Paperboat/ipc/u501-control.sock",
		"private/var/log/com.pinksaucepasta.paperboat.hostd.u501.log",
	)
	assertPresent(t, root,
		daemons+"com.pinksaucepasta.paperboat.hostd.u5010.plist",
		daemons+"io.paperboat.machineguard.plist",
		"Library/Application Support/Paperboat/ipc/u5010-control.sock",
		"private/var/log/com.pinksaucepasta.paperboat.hostd.u5010.log",
	)
	if fake.guards != 1 {
		t.Fatalf("guard calls=%d want 1", fake.guards)
	}
}

func TestPurgeSharedPayloadParentLifetime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("shared payload custody fixture requires root")
	}
	for _, state := range []string{"empty", "other-account", "foreign", "reset", "unsafe", "symlink", "darwin-empty-ipc", "darwin-foreign-ipc", "darwin-other-record", "darwin-unsafe-ipc"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			plan, err := newUnixPurgePlan("linux", 1000)
			if err != nil {
				t.Fatal(err)
			}
			parent := "/usr/local/libexec/paperboat/users"
			ipc := ""
			if strings.HasPrefix(state, "darwin-") {
				plan, err = newUnixPurgePlan("darwin", 501)
				if err != nil {
					t.Fatal(err)
				}
				parent = "/Library/Application Support/Paperboat/users"
				ipc = "/Library/Application Support/Paperboat/ipc"
				plantDir(t, root, ipc)
			}
			shared := filepath.Dir(parent)
			if err := os.MkdirAll(filepath.Join(root, parent), 0755); err != nil {
				t.Fatal(err)
			}
			s := testSweep(root, plan, &purgeFake{})
			switch state {
			case "other-account":
				if err := os.Mkdir(filepath.Join(root, parent, "u1001"), 0755); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				if err := os.WriteFile(filepath.Join(root, shared, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "reset":
				s.keepCLI = "/usr/local/libexec/paperboat/users/u1000/bin/pb"
			case "unsafe":
				if err := os.Chmod(filepath.Join(root, parent), 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filepath.Join(root, parent)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root, filepath.Join(root, parent)); err != nil {
					t.Fatal(err)
				}
			case "darwin-foreign-ipc":
				plantFile(t, root, filepath.Join(ipc, "foreign"))
			case "darwin-other-record":
				plantFile(t, root, filepath.Join(parent, "u502", "install-metadata.json"))
			case "darwin-unsafe-ipc":
				if err := os.Chmod(filepath.Join(root, ipc), 0777); err != nil {
					t.Fatal(err)
				}
			}
			err = s.removeEmptySharedPayloadParents(context.Background())
			if state == "unsafe" || state == "symlink" || state == "darwin-unsafe-ipc" {
				if err == nil {
					t.Fatal("unsafe shared parent accepted")
				}
				assertPresent(t, root, parent)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if state == "empty" || state == "darwin-empty-ipc" {
				assertGone(t, root, parent, shared)
				if ipc != "" {
					assertGone(t, root, ipc)
				}
			} else {
				assertPresent(t, root, shared)
				if state != "foreign" && state != "darwin-foreign-ipc" {
					assertPresent(t, root, parent)
				}
				if ipc != "" {
					assertPresent(t, root, ipc)
				}
			}
		})
	}
}

func TestDarwinPurgeSharedManualLifetime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned manual custody fixture requires root")
	}
	for _, remaining := range []string{"", "cli", "runtime", "reset", "unsafe"} {
		t.Run(remaining, func(t *testing.T) {
			root := t.TempDir()
			manualRoot := filepath.Join(root, "usr/local/share/man")
			if err := documentation.Install(manualRoot); err != nil {
				t.Fatal(err)
			}
			pages, err := filepath.Glob(filepath.Join(manualRoot, "man1", "pb*.1"))
			if err != nil || len(pages) == 0 {
				t.Fatal("bundled manual fixture missing", err)
			}
			foreign := filepath.Join(manualRoot, "man1", "pb-foreign.1")
			if err := os.WriteFile(foreign, []byte("foreign manual\n"), 0644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(manualRoot, "man1", "pb-link.1")
			if err := os.Symlink(pages[0], link); err != nil {
				t.Fatal(err)
			}
			plan, err := newUnixPurgePlan("darwin", 501)
			if err != nil {
				t.Fatal(err)
			}
			sweep := testSweep(root, plan, newPurgeFake())
			var warnings []string
			sweep.warnings = &warnings
			switch remaining {
			case "cli":
				plantFile(t, root, "Library/PrivilegedHelperTools/Paperboat/users/u502/bin/pb")
			case "runtime":
				plantFile(t, root, "Library/Application Support/Paperboat/users/u502/install-metadata.json")
			case "reset":
				sweep.keepCLI = "/Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb"
				plantFile(t, root, sweep.keepCLI)
			case "unsafe":
				if err := os.Chmod(filepath.Join(manualRoot, "man1"), 0777); err != nil {
					t.Fatal(err)
				}
			}
			if err = sweep.purgeAccount(context.Background()); err != nil {
				t.Fatal(err)
			}
			if remaining == "unsafe" && len(warnings) != 1 {
				t.Fatalf("unsafe shared directory was not reported as a warning: %v", warnings)
			}
			for _, page := range pages {
				_, err := os.Lstat(page)
				if remaining == "" && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("last-account uninstall retained %s: %v", filepath.Base(page), err)
				}
				if remaining != "" && err != nil {
					t.Fatalf("shared manual removed while %s retained: %v", remaining, err)
				}
			}
			assertPresent(t, root, "/usr/local/share/man/man1/pb-foreign.1", "/usr/local/share/man/man1/pb-link.1")
		})
	}
}

func TestOwnsInstanceRequiresExactAccountToken(t *testing.T) {
	for name, want := range map[string]bool{
		"paperboat-hostd-u1001.service":                    true,
		"paperboat-session-owner-u1001-g1.service":         true,
		"com.pinksaucepasta.paperboat.carrier.u1001.plist": true,
		"u1001-control.sock":                               true,
		"paperboat-hostd-u10010.service":                   false,
		"paperboat-hostd-u100.service":                     false,
		"paperboat-hostd-u1002.service":                    false,
		"paperboat-hostd-u21001.service":                   false,
	} {
		if got := ownsInstance(name, "u1001"); got != want {
			t.Fatalf("ownsInstance(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLinuxParentPIDSurvivesHostileCommandNames(t *testing.T) {
	if got := linuxParentPID([]byte("123 (my (odd) name) S 45 1 1 0 -1")); got != 45 {
		t.Fatalf("ppid = %d, want 45", got)
	}
}

func TestDarwinProcessParsingKeepsSpacedExecutablePaths(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	processes := parseDarwinProcesses("  501     1 1-02:03:04 /Library/Application Support/Paperboat/users/u501/bin/pb\n  502 1 05:06 /usr/sbin/x\n\n  x y z w\n", now)
	want := []unixProcess{
		{pid: 501, ppid: 1, exe: "/Library/Application Support/Paperboat/users/u501/bin/pb", started: now.Add(-(26*time.Hour + 3*time.Minute + 4*time.Second))},
		{pid: 502, ppid: 1, exe: "/usr/sbin/x", started: now.Add(-(5*time.Minute + 6*time.Second))},
	}
	if !reflect.DeepEqual(processes, want) {
		t.Fatalf("processes = %+v, want %+v", processes, want)
	}
}

func TestPurgePlanRejectsUnsupportedInputs(t *testing.T) {
	if _, err := newUnixPurgePlan("linux", -1); err == nil {
		t.Fatal("negative uid accepted")
	}
	if _, err := newUnixPurgePlan("windows", 1001); err == nil {
		t.Fatal("windows accepted by the unix plan")
	}
}

func TestResetSweepPreservesCLIAndRejectsOtherPayload(t *testing.T) {
	root := t.TempDir()
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	keep := "/usr/local/libexec/paperboat/users/u1001/bin/pb"
	plantFile(t, root, keep)
	plantFile(t, root, "/usr/local/libexec/paperboat/users/u1001/releases/old")
	fake := newPurgeFake()
	sweep := testSweep(root, plan, fake)
	sweep.keepCLI = keep
	if err := sweep.sweepAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sweep.at(keep)); err != nil {
		t.Fatal("reset deleted CLI", err)
	}
	leftovers, err := sweep.leftovers(context.Background())
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("reset leftovers: %v %v", leftovers, err)
	}
	plantFile(t, root, "/usr/local/libexec/paperboat/users/u1001/unexpected")
	leftovers, err = sweep.leftovers(context.Background())
	if err != nil || len(leftovers) == 0 {
		t.Fatal("reset accepted extra payload", err)
	}
}

// F1-09 device finding: uninstall left /run/paperboat after the last account.
func TestPurgeRemovesSharedRuntimeDirectoriesWithTheLastAccountOnly(t *testing.T) {
	for _, accountsRemain := range []bool{false, true} {
		root := t.TempDir()
		plantLinuxAccount(t, root)
		runtimeDirectory := filepath.Join(root, "run", "paperboat")
		if err := os.MkdirAll(runtimeDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeDirectory, "host-service.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := newUnixPurgePlan("linux", 1001)
		if err != nil {
			t.Fatal(err)
		}
		if accountsRemain {
			other := filepath.Join(root, plan.accountRecords, "u1002")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "install-metadata.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_ = testSweep(root, plan, newPurgeFake()).purgeAccount(context.Background())
		_, statErr := os.Stat(runtimeDirectory)
		if accountsRemain && statErr != nil {
			t.Fatalf("another account still needs %s: %v", runtimeDirectory, statErr)
		}
		if !accountsRemain && !os.IsNotExist(statErr) {
			t.Fatalf("shared runtime directory survived the last uninstall: %v", statErr)
		}
	}
}

// A user's own pb (pb connect, exec, ssh, send) of the installed version runs
// from the account payload but outside our units: uninstall and reset stop the
// units and their main processes, never it, whatever its executable.
func TestPurgeAndResetNeverKillAUsersOwnPB(t *testing.T) {
	const pb = "/usr/local/libexec/paperboat/users/u1001/bin/pb"
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			root := t.TempDir()
			plantLinuxAccount(t, root)
			plan, err := newUnixPurgePlan("linux", 1001)
			if err != nil {
				t.Fatal(err)
			}
			fake := newPurgeFake(
				unixProcess{pid: 4001, ppid: 1, exe: pb},                // hostd, still running after its unit stop
				unixProcess{pid: 4002, ppid: 4001, exe: "/bin/bash"},    // a Paperboat terminal's shell
				unixProcess{pid: 4003, ppid: 4002, exe: pb},             // pb connect typed in it
				unixProcess{pid: 4004, ppid: 1, exe: pb, stale: true},   // nohup pb exec, reparented to init
				unixProcess{pid: 4005, ppid: 900, exe: pb, stale: true}, // pb ssh in a login shell
			)
			fake.jobs[4001] = "paperboat-hostd-u1001.service"
			fake.termExits[4001] = true
			sweep := testSweep(root, plan, fake)
			if reset {
				err = sweep.purgeForReset(context.Background(), func() (string, error) { return pb, nil }, func(context.Context, string) error { return nil })
			} else {
				err = sweep.purgeAccount(context.Background())
			}
			if err != nil {
				t.Fatalf("purge failed:\n%s", errorChainText(err))
			}
			if want := []string{"terminated 4001"}; !reflect.DeepEqual(fake.signals, want) {
				t.Fatalf("signals = %v, want only the unit's main process %v", fake.signals, want)
			}
			for _, pid := range []int{4003, 4004, 4005} {
				if _, alive := fake.alive[pid]; !alive {
					t.Fatalf("user process %d was killed", pid)
				}
			}
		})
	}
}

// A machine whose lifecycle state is corrupt (service-lifecycle.json unreadable) must still be removable: the native
// uninstall cannot say what to stop, so the sweep removes everything Paperboat owns by its known names, the final
// inventory decides success, and the removed items are reported. Any other native failure still fails.
func TestPurgeRemovesByNameWhenTheLifecycleStateIsCorrupt(t *testing.T) {
	for _, cause := range []error{service.ErrLifecycleUncertain, service.ErrLifecycleInvalid} {
		root := t.TempDir()
		plantLinuxAccount(t, root)
		plan, err := newUnixPurgePlan("linux", 1001)
		if err != nil {
			t.Fatal(err)
		}
		fake := newPurgeFake(unixProcess{pid: 4001, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/pb"})
		fake.jobs[4001] = "paperboat-hostd-u1001.service"
		fake.uninstallErr = withPurgeStage("uninstall_recover", errors.Join(cause, errors.New("decode service-lifecycle.json")))
		var removed []string
		sweep := testSweep(root, plan, fake)
		sweep.removedByName = &removed
		if err := sweep.purgeAccount(context.Background()); err != nil {
			t.Fatalf("a corrupt lifecycle state (%v) blocked the uninstall: %v", cause, err)
		}
		assertGone(t, root, "etc/systemd/system/paperboat-hostd-u1001.service", "var/lib/paperboat/users/u1001", "var/lib/paperboat-installer/users/u1001/install-metadata.json", "usr/local/libexec/paperboat/users/u1001")
		assertPresent(t, root, "usr/local/libexec/paperboat/users/u10010/bin/pb", "etc/systemd/system/paperboat-hostd-u1002.service")
		if len(fake.alive) != 0 {
			t.Fatalf("a Paperboat process still runs: %v", fake.alive)
		}
		for _, want := range []string{"/etc/systemd/system/paperboat-hostd-u1001.service", "/var/lib/paperboat-installer/users/u1001", "process 4001 (/usr/local/libexec/paperboat/users/u1001/bin/pb)"} {
			if !slices.Contains(removed, want) {
				t.Fatalf("the removal report lacks %q: %v", want, removed)
			}
		}
	}
}

// The by-name removal never hides what it could not remove: a process that survives is still a leftover, so the
// uninstall fails and names it.
func TestPurgeAfterACorruptLifecycleStillFailsOnALeftover(t *testing.T) {
	root := t.TempDir()
	plantLinuxAccount(t, root)
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	fake := newPurgeFake(unixProcess{pid: 4005, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/session-owner"})
	fake.jobs[4005] = "paperboat-session-owner-u1001-g2.service"
	fake.immortal[4005] = true
	fake.uninstallErr = errors.Join(service.ErrLifecycleUncertain, errors.New("corrupt"))
	var removed []string
	sweep := testSweep(root, plan, fake)
	sweep.removedByName = &removed
	err = sweep.purgeAccount(context.Background())
	if err == nil || !strings.Contains(errorChainText(err), "process 4005") {
		t.Fatalf("a surviving process was not reported: %v", err)
	}
}

// agents read removed_by_name: every path or process is named once, however many lists of the plan contain it.
func TestPurgeRemovedByNameNamesEachItemOnce(t *testing.T) {
	root := t.TempDir()
	plantLinuxAccount(t, root)
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	// The same definition and payload named by several lists of the plan, and two processes of one executable.
	plan.definitions = append(plan.definitions, plan.definitions...)
	plan.payloadPaths = append(plan.payloadPaths, plan.payloadPaths...)
	fake := newPurgeFake(
		unixProcess{pid: 4001, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/pb"},
		unixProcess{pid: 4002, ppid: 1, exe: "/usr/local/libexec/paperboat/users/u1001/bin/pb"})
	fake.jobs[4001], fake.jobs[4002] = "paperboat-hostd-u1001.service", "paperboat-hostd-u1001.service"
	fake.uninstallErr = errors.Join(service.ErrLifecycleUncertain, errors.New("corrupt"))
	var removed []string
	sweep := testSweep(root, plan, fake)
	sweep.removedByName = &removed
	if err := sweep.purgeAccount(context.Background()); err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	seen := map[string]bool{}
	for _, item := range removed {
		if seen[item] {
			t.Errorf("%q is listed twice: %v", item, removed)
		}
		seen[item] = true
		if !validPurgeRemaining(item) {
			t.Errorf("%q is not a path or \"process <pid> (<path>)\"", item)
		}
	}
	for _, want := range []string{"process 4001 (/usr/local/libexec/paperboat/users/u1001/bin/pb)", "process 4002 (/usr/local/libexec/paperboat/users/u1001/bin/pb)"} {
		if !seen[want] {
			t.Errorf("the report lacks %q: %v", want, removed)
		}
	}
}

// A shared-manuals custody refusal never stops the purge of the user's own data: it is a named warning with
// what the user can do. Stock macOS layouts make the parents group-writable (Intel Homebrew: /usr/local/share
// is user:admin, mode 775) and those parents only have to be real directories.
func TestDarwinPurgeSharedManualCustodyNeverBlocksTheUsersData(t *testing.T) {
	for name, tc := range map[string]struct {
		shareMode, man1Mode os.FileMode
		wantWarning         bool
	}{
		"group-writable parent is allowed":   {0775, 0755, false},
		"group-writable man1 is a warning":   {0755, 0775, true},
		"world-writable man1 is a warning":   {0755, 0777, true},
		"group-writable share and man1 warn": {0775, 0775, true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manualRoot := filepath.Join(root, "usr/local/share/man")
			if err := documentation.Install(manualRoot); err != nil {
				t.Fatal(err)
			}
			for path, mode := range map[string]os.FileMode{
				filepath.Join(root, "usr/local/share"): tc.shareMode, manualRoot: 0755, filepath.Join(manualRoot, "man1"): tc.man1Mode,
			} {
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			pages, err := filepath.Glob(filepath.Join(manualRoot, "man1", "pb*.1"))
			if err != nil || len(pages) == 0 {
				t.Fatal("bundled manual fixture missing", err)
			}
			plan, err := newUnixPurgePlan("darwin", 501)
			if err != nil {
				t.Fatal(err)
			}
			userData := "Library/Application Support/Paperboat/users/u501/state"
			plantFile(t, root, userData)
			var warnings []string
			sweep := testSweep(root, plan, newPurgeFake())
			sweep.warnings = &warnings
			if err := sweep.purgeAccount(context.Background()); err != nil {
				t.Fatalf("purge failed because of the shared manuals: %v", err)
			}
			if tc.wantWarning != (len(warnings) == 1 && warnings[0] == purgeWarningSharedManuals) || !tc.wantWarning && len(warnings) != 0 {
				t.Fatalf("warnings = %v, want warning=%v", warnings, tc.wantWarning)
			}
			for _, page := range pages {
				_, err := os.Lstat(page)
				if tc.wantWarning && err != nil {
					t.Fatalf("an unsafe directory lost %s: %v", filepath.Base(page), err)
				}
				if !tc.wantWarning && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the manual %s was kept in a safe layout: %v", filepath.Base(page), err)
				}
			}
		})
	}
}

func TestPurgeResultCarriesTheManualWarningAndRefusesAnyOther(t *testing.T) {
	var output bytes.Buffer
	if err := writePurgeReport(t.Context(), &output, nil, nil, []string{purgeWarningSharedManuals, "other"}); err != nil {
		t.Fatal(err)
	}
	removed, warnings, err := readPurgeSummary(output.Bytes(), nil)
	if err != nil || len(removed) != 0 || len(warnings) != 1 || warnings[0] != purgeWarningSharedManuals {
		t.Fatalf("removed=%v warnings=%v err=%v", removed, warnings, err)
	}
	forged := []byte(`{"version":"paperboat.purge-result/v1","ok":true,"warnings":["delete_everything"]}`)
	if _, _, err := readPurgeSummary(forged, nil); err == nil {
		t.Fatal("an unknown warning was accepted")
	}
}
