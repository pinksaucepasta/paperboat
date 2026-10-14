//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// purgeFake stands in for systemctl, launchctl, the process table, signals and
// the native uninstall and guard steps. Processes that exit on SIGTERM are
// removed from the table then; immortal ones survive SIGKILL.
type purgeFake struct {
	commands     []string
	signals      []string
	alive        map[int]unixProcess
	termExits    map[int]bool
	immortal     map[int]bool
	failCommands map[string]error
	uninstallErr error
	uninstalls   int
	guards       int
}

func newPurgeFake(processes ...unixProcess) *purgeFake {
	fake := &purgeFake{alive: map[int]unixProcess{}, termExits: map[int]bool{}, immortal: map[int]bool{}, failCommands: map[string]error{}}
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
	assertGone(t, root,
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
	assertPresent(t, root,
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

func TestFullPurgeRetiresOnlyOwnedLegacyAvailabilityPolicy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned legacy policy fixture requires root")
	}
	for _, kind := range []string{"owned", "foreign-content", "wrong-schema", "symlink", "hardlink", "other-account", "reset", "failed-stop"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			plan, err := newUnixPurgePlan("linux", 1000)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "var/lib/paperboat/availability-policy.json")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"schema":"paperboat.host-service/v1","desired_mode":"allow_sleep","desired_version":1,"status":"applied"}`)
			if kind == "wrong-schema" {
				body = []byte(`{"schema":"foreign"}`)
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			fake := newPurgeFake()
			s := testSweep(root, plan, fake)
			switch kind {
			case "foreign-content":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
			case "other-account":
				plantFile(t, root, "var/lib/paperboat-installer/users/u1001/install-metadata.json")
			case "reset":
				s.keepCLI = "/usr/local/libexec/paperboat/users/u1000/bin/pb"
			case "failed-stop":
				plantFile(t, root, "var/lib/paperboat-installer/users/u1000/install-metadata.json")
				fake.uninstallErr = syscall.EIO
			}
			err = s.purgeAccount(context.Background())
			refuse := kind == "wrong-schema" || kind == "symlink" || kind == "hardlink" || kind == "failed-stop"
			if (err != nil) != refuse {
				t.Fatalf("error=%v refuse=%v", err, refuse)
			}
			if refuse || kind == "other-account" || kind == "reset" {
				assertPresent(t, root, "/var/lib/paperboat/availability-policy.json")
			} else {
				assertGone(t, root, "/var/lib/paperboat/availability-policy.json")
			}
			if kind == "foreign-content" {
				assertPresent(t, root, "/var/lib/paperboat/foreign")
			}
		})
	}
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
			err = sweep.purgeAccount(context.Background())
			if remaining == "unsafe" && err == nil {
				t.Fatal("unsafe shared directory accepted")
			}
			if remaining != "unsafe" && err != nil {
				t.Fatal(err)
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
	processes := parseDarwinProcesses("  501     1 /Library/Application Support/Paperboat/users/u501/bin/pb\n\n  x y z\n")
	want := []unixProcess{{pid: 501, ppid: 1, exe: "/Library/Application Support/Paperboat/users/u501/bin/pb"}}
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
