//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

func runPurgeCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	_, err := runPurge(ctx, args, stdin, stdout, stderr)
	return err
}

// runPurge is runPurgeCommand that also returns what the cleanup removed by known names after an unreadable
// lifecycle state (and prints it to stderr).
func runPurge(ctx context.Context, args []string, _ io.Reader, _, stderr io.Writer) ([]string, error) {
	if len(args) != 0 && (len(args) != 1 || args[0] != "--preserve-cli") {
		return nil, errors.New("purge accepts no arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	arguments := append([]string{"--", "/usr/bin/env", "PAPERBOAT_INVOKING_UID=" + strconv.Itoa(os.Getuid()), executable, "__runtime-service", "purge"}, args...)
	command := sudoCommand(ctx, arguments...)
	command.Stderr = io.Discard
	var output purgeOutput
	command.Stdout = &output
	err = command.Run()
	if output.exceeded {
		return nil, runtimeFailure("privileged cleanup result exceeded its bound", err)
	}
	removed, warnings, err := readPurgeSummary(output.data, errors.Join(err, runtimeContextFailure(ctx)))
	if len(warnings) > 0 {
		fmt.Fprintln(stderr, "Paperboat's own files were removed, but its shared manual pages in /usr/local/share/man/man1 were left because that directory is not safe to clean as root. Remove them yourself with: sudo rm -f /usr/local/share/man/man1/pb.1 /usr/local/share/man/man1/pb-*.1")
	}
	if len(removed) > 0 {
		fmt.Fprintf(stderr, "Paperboat's service state could not be read, so everything it owns was removed by its known names: %s.\n", strings.Join(removed, ", "))
	}
	return removed, err
}

// purgeSystemInstallation is the root side of uninstall and reset. The order is
// the contract: native uninstall of this account when its metadata exists, an
// inventory sweep for this account, the machine guard only when no account
// remains installed, and finally a re-scan that names every leftover.
func purgeSystemInstallation(ctx context.Context) error {
	return purgeSystemInstallationScoped(ctx, false)
}

func purgeSystemInstallationScoped(ctx context.Context, preserveCLI bool) error {
	_, _, err := purgeSystemInstallationReport(ctx, preserveCLI)
	return err
}

// purgeSystemInstallationReport is purgeSystemInstallationScoped that also returns the Paperboat items it removed
// by their known names because the native lifecycle state could not be read (nil when it could).
func purgeSystemInstallationReport(ctx context.Context, preserveCLI bool) (removed, warnings []string, _ error) {
	if os.Geteuid() != 0 {
		return nil, nil, errors.New("complete uninstall requires administrator approval")
	}
	plan, err := newUnixPurgePlan(runtime.GOOS, invokingServiceUID())
	if err != nil {
		return nil, nil, err
	}
	sweep := newHostUnixSweep(plan)
	sweep.removedByName = &removed
	sweep.warnings = &warnings
	if preserveCLI {
		err = sweep.purgeForReset(ctx, hostinstall.ResetCLIForCurrentAccount, hostinstall.UninstallPersistedPreservingCLI)
		return removed, warnings, err
	}
	err = sweep.purgeAccount(ctx)
	return removed, warnings, err
}

// purgeForReset keeps the account CLI that prove accepts. A CLI it cannot
// prove never stops the rest of the reset: it is removed with everything else
// and named as the reset_cli failure, so reset cannot leave a stuck machine.
func (s unixSweep) purgeForReset(ctx context.Context, prove func() (string, error), uninstall func(context.Context, string) error) error {
	keep, err := prove()
	if err != nil {
		return errors.Join(withPurgeStage("reset_cli", err), s.purgeAccount(ctx))
	}
	s.keepCLI = keep
	s.uninstall = func(ctx context.Context) error { return uninstall(ctx, keep) }
	return s.purgeAccount(ctx)
}

// unixPurgePlan is the Paperboat-owned inventory for one enrolled account on a
// Unix host. Fixed paths are listed explicitly; generation-specific items
// (session owners, carriers, activators, drop-ins, links) are matched by glob
// and must carry this account's exact token.
type unixPurgePlan struct {
	platform string
	uid      int
	instance string
	// units are systemd unit names or launchd labels that always belong to
	// the account, whether or not their definitions still exist.
	units       []string
	definitions []string
	// definitionGlobs match unit files, drop-in directories, wants links, or
	// launchd plists for any generation of this account.
	definitionGlobs []string
	payloadPaths    []string
	payloadGlobs    []string
	// sharedRuntimeDirs are volatile directories shared by every account; they go
	// with the last installation.
	sharedRuntimeDirs []string
	// accountRecords holds one directory per installed account; its
	// install-metadata.json files decide whether the machine guard may go.
	accountRecords string
}

func newUnixPurgePlan(platform string, uid int) (unixPurgePlan, error) {
	if uid < 0 {
		return unixPurgePlan{}, errors.New("invalid enrolled user")
	}
	instance := "u" + strconv.Itoa(uid)
	switch platform {
	case "linux":
		units := []string{
			"paperboat-runtime-privileged-" + instance + ".service",
			"paperboat-hostd-" + instance + ".service",
			"paperboat-updated-" + instance + ".service",
		}
		return unixPurgePlan{
			platform:    platform,
			uid:         uid,
			instance:    instance,
			units:       units,
			definitions: appendSystemdDefinitions(units),
			definitionGlobs: []string{
				"/etc/systemd/system/paperboat-*-" + instance + "*",
				"/etc/systemd/system/*/paperboat-*-" + instance + "*",
			},
			payloadPaths: []string{
				filepath.Join("/usr/local/libexec/paperboat/users", instance),
				filepath.Join("/var/lib/paperboat-installer/users", instance),
				filepath.Join("/var/lib/paperboat/users", instance),
				"/var/lib/paperboat-updated-" + instance,
				"/var/run/paperboat-hostd-" + instance,
				"/var/run/paperboat-updated-" + instance,
			},
			payloadGlobs: []string{
				"/var/lib/paperboat*-" + instance + "*",
				"/run/paperboat-*-" + instance + "*",
				"/var/run/paperboat-*-" + instance + "*",
				"/var/cache/paperboat*-" + instance + "*",
			},
			sharedRuntimeDirs: []string{"/run/paperboat", "/var/run/paperboat"},
			accountRecords:    "/var/lib/paperboat-installer/users",
		}, nil
	case "darwin":
		labels := []string{service.HostLabel + "." + instance, service.HostdLabel + "." + instance, service.UpdaterLabel + "." + instance}
		definitions := make([]string, 0, len(labels))
		for _, label := range labels {
			definitions = append(definitions, filepath.Join("/Library", "LaunchDaemons", label+".plist"))
		}
		return unixPurgePlan{
			platform:    platform,
			uid:         uid,
			instance:    instance,
			units:       labels,
			definitions: definitions,
			definitionGlobs: []string{
				"/Library/LaunchDaemons/com.pinksaucepasta.paperboat.*." + instance + "*.plist",
			},
			payloadPaths: []string{
				filepath.Join("/Library/PrivilegedHelperTools/Paperboat/users", instance),
				filepath.Join("/Library/Application Support/Paperboat/users", instance),
				"/Library/Application Support/Paperboat/updated-" + instance,
				"/Library/Application Support/Paperboat/run/paperboat-hostd-" + instance,
				"/Library/Application Support/Paperboat/run/paperboat-updated-" + instance,
			},
			payloadGlobs: []string{
				"/Library/Application Support/Paperboat/run/paperboat-*-" + instance + "*",
				"/Library/Application Support/Paperboat/ipc/" + instance + "-*",
				"/private/var/log/com.pinksaucepasta.paperboat.*." + instance + "*.log",
			},
			sharedRuntimeDirs: []string{"/Library/Application Support/Paperboat/run"},
			accountRecords:    "/Library/Application Support/Paperboat/users",
		}, nil
	default:
		return unixPurgePlan{}, errors.New("unsupported purge platform")
	}
}

func appendSystemdDefinitions(units []string) []string {
	definitions := make([]string, 0, len(units)+1)
	for _, unit := range units {
		definitions = append(definitions, filepath.Join("/etc", "systemd", "system", unit))
	}
	definitions = append(definitions, "/etc/systemd/system/paperboat-helper.service.d")
	return definitions
}

func (p unixPurgePlan) metadataPath() string {
	return filepath.Join(p.accountRecords, p.instance, "install-metadata.json")
}

// ownsInstance reports whether name carries this account's exact token. A
// plain glob for u1001 also matches u10010, so the token must start at a name
// boundary and must not continue with another digit.
func ownsInstance(name, instance string) bool {
	for start := 0; start < len(name); {
		index := strings.Index(name[start:], instance)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(instance)
		startOK := index == 0 || name[index-1] == '-' || name[index-1] == '.'
		endOK := end == len(name) || name[end] < '0' || name[end] > '9'
		if startOK && endOK {
			return true
		}
		start = index + 1
	}
	return false
}

// unixProcess is one host process. exe is the resolved executable path.
type unixProcess struct {
	pid  int
	ppid int
	exe  string
	// started is when the process started. stale reports (Linux) that the
	// image it runs is not the file now at exe: another version's.
	started time.Time
	stale   bool
	// unit is the systemd unit or launchd job whose main process this is; the
	// inventory sets it from the service manager, never from the exe.
	unit string
}

// unixSweep performs one account's teardown. Paths in the plan are canonical
// host paths; root prefixes every filesystem operation so tests run in a temp
// tree. Every external effect is injected.
type unixSweep struct {
	root      string
	keepCLI   string
	plan      unixPurgePlan
	run       func(context.Context, string, ...string) error
	remove    func(string) error
	processes func(context.Context) ([]unixProcess, error)
	signal    func(pid int, sig syscall.Signal) error
	uninstall func(context.Context) error
	guard     func(context.Context) error
	sources   func(context.Context) (hostinstall.InventorySources, error)
	// jobs maps the main pid of each running unit or launchd job to its name.
	jobs func(context.Context) (map[int]string, error)
	// imageInodes lists the inodes a process maps as text (macOS); nil when
	// the process listing already compared images (Linux).
	imageInodes   func(context.Context, int) ([]uint64, error)
	clock         func() time.Time
	terminateWait time.Duration
	killWait      time.Duration
	// removedByName, when set, receives what the sweep removed after the native lifecycle state could not be read.
	removedByName *[]string
	// warnings, when set, receives the cleanups that did not finish but never block the user's own data
	// (named by purgeWarning*). A warning is reported to the user next to the success.
	warnings *[]string
}

// purgeWarningSharedManuals names the shared manual pages the purge could not prove safe to remove.
const purgeWarningSharedManuals = "shared_manuals"

const unixPurgePollInterval = 50 * time.Millisecond

func newHostUnixSweep(plan unixPurgePlan) unixSweep {
	processes := listDarwinProcesses
	if plan.platform == "linux" {
		processes = listLinuxProcesses
	}
	return unixSweep{
		plan: plan,
		run: func(ctx context.Context, name string, arguments ...string) error {
			return exec.CommandContext(ctx, name, arguments...).Run()
		},
		remove:    os.RemoveAll,
		processes: processes,
		signal: func(pid int, sig syscall.Signal) error {
			return syscall.Kill(pid, sig)
		},
		uninstall: hostinstall.UninstallPersisted,
		guard:     machineguard.Purge,
		sources: func(ctx context.Context) (hostinstall.InventorySources, error) {
			return hostinstall.LoadInventorySources(ctx, plan.uid)
		},
		jobs:          hostJobs(plan),
		imageInodes:   hostImageInodes(plan),
		terminateWait: 5 * time.Second,
		killWait:      2 * time.Second,
	}
}

func (s unixSweep) at(path string) string { return filepath.Join(s.root, path) }

func (s unixSweep) removePath(path string) error { return s.remove(s.at(path)) }

// purgeAccount runs the ordered teardown and returns one failure naming every
// step that could not be confirmed.
func (s unixSweep) purgeAccount(ctx context.Context) error {
	var result error
	metadataPresent, err := s.exists(s.plan.metadataPath())
	if err != nil {
		result = errors.Join(result, withPurgeStage("final_inventory", err))
	}
	var inventory []string
	lifecycleUnknown := false
	if metadataPresent {
		if err := s.uninstall(ctx); errors.Is(err, service.ErrLifecycleUncertain) || errors.Is(err, service.ErrLifecycleInvalid) {
			// The lifecycle state is unreadable or corrupt, so what it would have stopped is unknown: the sweep
			// below removes everything Paperboat owns by its known names, and the final inventory decides success.
			lifecycleUnknown = true
			inventory, _ = s.leftovers(ctx)
		} else if err != nil {
			result = errors.Join(result, withPurgeStage("native_uninstall", err))
		}
	}

	result = errors.Join(result, withPurgeStage("account_sweep", s.sweepAccount(ctx)))

	// The machine guard is shared by every account, so it goes only when no
	// install record remains. An unreadable record set keeps the guard.
	recordsRemain, err := s.recordsRemain()
	result = errors.Join(result, withPurgeStage("final_inventory", err))
	if err == nil && !recordsRemain {
		if err := s.guard(ctx); err != nil {
			result = errors.Join(result, withPurgeStage("machine_guard", err))
		}
		for _, directory := range s.plan.sharedRuntimeDirs {
			if err := s.removePath(directory); err != nil {
				result = errors.Join(result, withPurgeStage("shared_runtime", err))
			}
		}
	}
	if s.plan.platform == "darwin" && s.keepCLI == "" {
		// The pages are shared with the machine, not the user's data: a custody refusal is a warning, never a
		// failure that stops the user's own credentials, runtime and state from being removed.
		if err := s.removeUnneededSharedManuals(); err != nil && s.warnings != nil {
			*s.warnings = append(*s.warnings, purgeWarningSharedManuals)
		}
	}
	if s.keepCLI == "" {
		result = errors.Join(result, withPurgeStage("shared_payload_parents", s.removeEmptySharedPayloadParents(ctx)))
	}

	leftovers, err := s.leftovers(ctx)
	result = errors.Join(result, withPurgeStage("final_inventory", err))
	if lifecycleUnknown && s.removedByName != nil {
		still := map[string]bool{}
		for _, item := range leftovers {
			still[item] = true
		}
		for _, item := range inventory {
			if !still[item] {
				*s.removedByName = append(*s.removedByName, item)
			}
		}
	}
	if len(leftovers) > 0 {
		result = errors.Join(result, withPurgeStage("final_inventory", unixLeftoverError{items: leftovers}))
	}
	return runtimeFailureIfAny("removing the Paperboat installation failed", result)
}

// Only the shared parents of this account's authoritative payload slot can
// become unnecessary. Other accounts and foreign contents keep them alive.
func (s unixSweep) removeEmptySharedPayloadParents(ctx context.Context) error {
	if s.keepCLI != "" {
		return nil
	}
	if s.plan.platform == "darwin" {
		remain, err := s.recordsRemain()
		if err != nil {
			return err
		}
		if !remain {
			ipc := filepath.Join(filepath.Dir(s.plan.accountRecords), "ipc")
			if err := s.removeEmptySharedDirectory(ctx, ipc); err != nil {
				return err
			}
		}
	}
	for _, payload := range s.plan.payloadPaths {
		if filepath.Base(payload) != s.plan.instance || filepath.Base(filepath.Dir(payload)) != "users" {
			continue
		}
		parent := filepath.Dir(payload)
		for _, directory := range []string{parent, filepath.Dir(parent)} {
			if err := s.removeEmptySharedDirectory(ctx, directory); err != nil {
				return err
			}
		}
	}
	return nil
}

// Only callers' canonical shared directories are eligible. Remove never
// follows a symlink, repairs custody, or recursively deletes foreign contents.
func (s unixSweep) removeEmptySharedDirectory(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.at(directory)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	boundary := s.at("/")
	relative, err := filepath.Rel(boundary, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("shared payload parent is outside its namespace")
	}
	current := boundary
	components := append([]string{""}, strings.Split(relative, string(filepath.Separator))...)
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("shared payload parent custody is unsafe")
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
}

// The PKG's manuals belong to the shared CLI lifetime. A reset retains its
// CLI, and any other account payload keeps these pages even if its enrollment
// record is absent or its runtime is temporarily stopped.
func (s unixSweep) removeUnneededSharedManuals() error {
	for _, parent := range []string{"/Library/PrivilegedHelperTools/Paperboat/users", s.plan.accountRecords} {
		entries, err := os.ReadDir(s.at(parent))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return nil
		}
	}
	directory := "/usr/local/share/man"
	section := s.at(filepath.Join(directory, "man1"))
	if _, err := os.Lstat(section); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// Check the path without creating or repairing shared filesystem paths. Purge runs as root, so the
	// effective uid is root here; tests run it unprivileged. The root prefix is the existing sweep's isolated
	// filesystem fixture. Only the manual tree this removes from (man, man1) must be owned by the purging
	// user and closed to group and others; the parents above it only have to be real directories, because
	// stock layouts make them group-writable (Intel Homebrew: /usr/local/share is user:admin, mode 775) and
	// nothing is written there.
	current := s.at("/")
	for _, component := range []string{"", "usr", "local", "share", "man", "man1"} {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		strict := component == "man" || component == "man1"
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || strict && (info.Mode().Perm()&0022 != 0 || int(stat.Uid) != os.Geteuid()) {
			return errors.New("shared manual directory custody is unsafe")
		}
	}
	entries, err := os.ReadDir(section)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if (name != "pb.1" && (!strings.HasPrefix(name, "pb-") || !strings.HasSuffix(name, ".1"))) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
			return errors.New("shared manual file custody is unsafe")
		}
	}
	// The documentation owner checks its exact v1 marker and preserves foreign
	// files and symlinks; do not duplicate that ownership parser here.
	return documentation.Remove(s.at(directory))
}

// unixLeftoverError names every item the re-scan still found after removal.
type unixLeftoverError struct{ items []string }

func (e unixLeftoverError) Error() string {
	return "Paperboat items remain after removal: " + strings.Join(e.items, ", ")
}

// sweepAccount stops and removes every unit, definition and payload that
// matches this account, then kills processes started from its payload roots.
func (s unixSweep) sweepAccount(ctx context.Context) error {
	var result error
	definitions, err := s.matchedDefinitions()
	result = errors.Join(result, err)
	units := s.accountUnits(definitions)
	s.stopUnits(ctx, units)
	for _, path := range append(append([]string(nil), s.plan.definitions...), definitions...) {
		result = errors.Join(result, s.removePath(path))
	}
	result = errors.Join(result, s.reloadUnits(ctx, units))
	result = errors.Join(result, s.stopProcesses(ctx))
	payloads, err := s.payloadPaths()
	result = errors.Join(result, err)
	for _, path := range payloads {
		if s.keepCLI != "" {
			result = errors.Join(result, hostinstall.RemovePathPreservingCLI(ctx, s.at(path), s.at(s.keepCLI)))
		} else {
			result = errors.Join(result, s.removePath(path))
		}
	}
	return result
}

// stopUnits asks the service manager to stop units, so it stops every process
// in their cgroup or job. A unit that never loaded or is already gone reports
// an error; absence is confirmed by the caller's re-scan.
func (s unixSweep) stopUnits(ctx context.Context, units []string) {
	if s.plan.platform == "linux" {
		for _, unit := range units {
			for _, action := range []string{"stop", "disable"} {
				if err := s.run(ctx, "/usr/bin/systemctl", action, unit); err != nil {
					observeRuntimeFailure(ctx, "uninstall", runtimeFailure("service cleanup could not be confirmed", err))
				}
			}
		}
		return
	}
	for _, label := range units {
		if err := s.run(ctx, "/bin/launchctl", "bootout", "system/"+label); err != nil {
			observeRuntimeFailure(ctx, "uninstall", runtimeFailure("service cleanup could not be confirmed", err))
		}
	}
}

// reloadUnits makes systemd forget removed definitions; launchd needs nothing.
func (s unixSweep) reloadUnits(ctx context.Context, units []string) error {
	if s.plan.platform != "linux" {
		return nil
	}
	err := s.run(ctx, "/usr/bin/systemctl", "daemon-reload")
	if err := s.run(ctx, "/usr/bin/systemctl", append([]string{"reset-failed"}, units...)...); err != nil {
		observeRuntimeFailure(ctx, "uninstall", runtimeFailure("service cleanup could not be confirmed", err))
	}
	return err
}

// accountUnits returns the systemd units or launchd labels to stop, in a
// stable order. Glob matches contribute their names, with drop-in suffixes
// removed.
func (s unixSweep) accountUnits(definitions []string) []string {
	seen := map[string]bool{}
	var units []string
	add := func(unit string) {
		if unit != "" && !seen[unit] {
			seen[unit] = true
			units = append(units, unit)
		}
	}
	for _, unit := range s.plan.units {
		add(unit)
	}
	for _, definition := range definitions {
		name := filepath.Base(definition)
		if s.plan.platform == "linux" {
			add(strings.TrimSuffix(name, ".d"))
		} else {
			add(strings.TrimSuffix(name, ".plist"))
		}
	}
	sort.Strings(units)
	return units
}

// matchedDefinitions returns the glob-matched native definitions for this
// account, excluding the fixed plan paths.
func (s unixSweep) matchedDefinitions() ([]string, error) {
	return s.matchPatterns(s.plan.definitionGlobs)
}

func (s unixSweep) payloadPaths() ([]string, error) {
	matches, err := s.matchPatterns(s.plan.payloadGlobs)
	paths := append(append([]string(nil), s.plan.payloadPaths...), matches...)
	return paths, err
}

// matchPatterns expands patterns below root. A match is kept only when its
// basename carries this account's exact token.
func (s unixSweep) matchPatterns(patterns []string) ([]string, error) {
	var result []string
	var failure error
	seen := map[string]bool{}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(s.at(pattern))
		if err != nil {
			failure = errors.Join(failure, err)
			continue
		}
		for _, match := range matches {
			canonical := strings.TrimPrefix(match, s.root)
			if seen[canonical] || !ownsInstance(filepath.Base(canonical), s.plan.instance) {
				continue
			}
			seen[canonical] = true
			result = append(result, canonical)
		}
	}
	sort.Strings(result)
	return result, failure
}

func (s unixSweep) exists(path string) (bool, error) {
	_, err := os.Lstat(s.at(path))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return true, err
}

// recordsRemain reports whether any account still has an install record. An
// error is returned so callers can fail closed.
func (s unixSweep) recordsRemain() (bool, error) {
	matches, err := filepath.Glob(s.at(filepath.Join(s.plan.accountRecords, "*", "install-metadata.json")))
	return len(matches) > 0, err
}

// accountProcesses returns the main processes of this account's units and
// jobs that run its payload (ownedProcesses). Uninstall and reset stop units
// first; this catches main processes that outlived their unit's stop.
func (s unixSweep) accountProcesses(ctx context.Context) ([]unixProcess, error) {
	return s.ownedProcesses(ctx, s.plan.payloadPaths)
}

// ownedProcesses returns the processes the service manager names as the main
// process of one of our units or jobs (systemd's MainPID, launchctl's job pid)
// and that run from roots, excluding this process and its ancestors. A process
// the user started, in any terminal or inside one of our units, is never in it,
// whatever its executable: the unit owns its children.
func (s unixSweep) ownedProcesses(ctx context.Context, roots []string) ([]unixProcess, error) {
	processes, err := s.processes(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := s.jobs(ctx)
	if err != nil {
		return nil, err
	}
	excluded := processTreeOf(processes, os.Getpid())
	var matches []unixProcess
	for _, process := range processes {
		unit, ok := jobs[process.pid]
		if !ok || !s.ourUnit(unit) || excluded[process.pid] || !underAnyRoot(process.exe, roots) {
			continue
		}
		process.unit = unit
		matches = append(matches, process)
	}
	return matches, nil
}

// processTreeOf returns self and its ancestors that are visible in processes.
func processTreeOf(processes []unixProcess, self int) map[int]bool {
	parent := make(map[int]int, len(processes))
	for _, process := range processes {
		parent[process.pid] = process.ppid
	}
	tree := map[int]bool{}
	for pid := self; pid > 1 && !tree[pid]; {
		tree[pid] = true
		next, ok := parent[pid]
		if !ok {
			break
		}
		pid = next
	}
	return tree
}

func underAnyRoot(exe string, roots []string) bool {
	for _, root := range roots {
		if exe == root || strings.HasPrefix(exe, root+"/") {
			return true
		}
	}
	return false
}

// stopProcesses sends SIGTERM, waits, then SIGKILL to survivors. Survivors that
// remain are reported by the re-scan, not hidden here.
func (s unixSweep) stopProcesses(ctx context.Context) error {
	targets, err := s.accountProcesses(ctx)
	if err != nil {
		return err
	}
	return s.stopTargets(ctx, targets)
}

func (s unixSweep) stopTargets(ctx context.Context, targets []unixProcess) error {
	var result error
	for _, process := range targets {
		result = errors.Join(result, s.signalProcess(process.pid, syscall.SIGTERM))
	}
	survivors, err := s.waitForProcesses(ctx, targets, s.terminateWait)
	result = errors.Join(result, err)
	for _, process := range survivors {
		result = errors.Join(result, s.signalProcess(process.pid, syscall.SIGKILL))
	}
	_, err = s.waitForProcesses(ctx, survivors, s.killWait)
	return errors.Join(result, err)
}

func (s unixSweep) signalProcess(pid int, sig syscall.Signal) error {
	if err := s.signal(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process %d: %w", pid, err)
	}
	return nil
}

// waitForProcesses polls until none of targets remains or wait elapses. It
// returns the targets still running.
func (s unixSweep) waitForProcesses(ctx context.Context, targets []unixProcess, wait time.Duration) ([]unixProcess, error) {
	deadline := time.Now().Add(wait)
	for {
		current, err := s.processes(ctx)
		if err != nil {
			return nil, err
		}
		var alive []unixProcess
		for _, target := range targets {
			for _, process := range current {
				if process.pid == target.pid && process.exe == target.exe {
					alive = append(alive, target)
					break
				}
			}
		}
		if len(alive) == 0 || !time.Now().Before(deadline) {
			return alive, nil
		}
		select {
		case <-ctx.Done():
			return alive, ctx.Err()
		case <-time.After(unixPurgePollInterval):
		}
		targets = alive
	}
}

// leftovers re-scans everything this account owns and returns each item still
// present, by path or by process.
func (s unixSweep) leftovers(ctx context.Context) ([]string, error) {
	var items []string
	var failure error
	definitions, err := s.matchedDefinitions()
	failure = errors.Join(failure, err)
	payloads, err := s.payloadPaths()
	failure = errors.Join(failure, err)
	for _, path := range append(append(append([]string(nil), s.plan.definitions...), definitions...), payloads...) {
		present, err := s.exists(path)
		failure = errors.Join(failure, err)
		if present && s.keepCLI != "" {
			only, inspectErr := hostinstall.CLIOnlyRemains(ctx, s.at(path), s.at(s.keepCLI))
			failure = errors.Join(failure, inspectErr)
			if only {
				continue
			}
		}
		// The plan's names and the matched definitions overlap, as do payload paths: name each item once.
		if present && !slices.Contains(items, path) {
			items = append(items, path)
		}
	}
	processes, err := s.accountProcesses(ctx)
	failure = errors.Join(failure, err)
	for _, process := range processes {
		if item := fmt.Sprintf("process %d (%s)", process.pid, process.exe); !slices.Contains(items, item) {
			items = append(items, item)
		}
	}
	return items, failure
}

func listLinuxProcesses(context.Context) ([]unixProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	boot := linuxBootTime()
	var processes []unixProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		directory := filepath.Join("/proc", entry.Name())
		exe, err := os.Readlink(filepath.Join(directory, "exe"))
		if err != nil {
			// The process exited or is a kernel thread; neither owns payload.
			continue
		}
		// A replaced binary keeps running as "<path> (deleted)".
		deleted := strings.HasSuffix(exe, " (deleted)")
		exe = strings.TrimSuffix(exe, " (deleted)")
		stat, err := os.ReadFile(filepath.Join(directory, "stat"))
		if err != nil {
			continue
		}
		process := unixProcess{pid: pid, ppid: linuxParentPID(stat), exe: exe, started: linuxStartTime(stat, boot), stale: deleted}
		if !deleted {
			image, imageErr := os.Stat(filepath.Join(directory, "exe"))
			current, currentErr := os.Stat(exe)
			process.stale = imageErr == nil && (currentErr != nil || !os.SameFile(image, current))
		}
		processes = append(processes, process)
	}
	return processes, nil
}

// linuxUserHZ is the kernel's fixed USER_HZ for /proc tick fields.
const linuxUserHZ = 100

func linuxBootTime() time.Time {
	data, _ := os.ReadFile("/proc/stat")
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			seconds, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			return time.Unix(seconds, 0)
		}
	}
	return time.Time{}
}

// linuxStartTime reads field 22 of /proc/<pid>/stat, in ticks since boot.
func linuxStartTime(stat []byte, boot time.Time) time.Time {
	text := string(stat)
	index := strings.LastIndexByte(text, ')')
	if index < 0 || boot.IsZero() {
		return time.Time{}
	}
	fields := strings.Fields(text[index+1:])
	if len(fields) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return boot.Add(time.Duration(ticks) * time.Second / linuxUserHZ)
}

// linuxParentPID reads field 4 of /proc/<pid>/stat. The command name may
// contain spaces and parentheses, so parsing starts after the last ')'.
func linuxParentPID(stat []byte) int {
	text := string(stat)
	index := strings.LastIndexByte(text, ')')
	if index < 0 {
		return 0
	}
	fields := strings.Fields(text[index+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// listDarwinProcesses reads ps output. On macOS comm is the executable path,
// which may contain spaces, so only the leading columns are split off.
func listDarwinProcesses(ctx context.Context) ([]unixProcess, error) {
	output, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,etime=,comm=").Output()
	if err != nil {
		return nil, err
	}
	return parseDarwinProcesses(string(output), time.Now()), nil
}

func parseDarwinProcesses(output string, now time.Time) []unixProcess {
	var processes []unixProcess
	for _, line := range strings.Split(output, "\n") {
		pidText, rest := cutField(line)
		ppidText, rest := cutField(rest)
		elapsedText, exe := cutField(rest)
		pid, pidErr := strconv.Atoi(pidText)
		ppid, ppidErr := strconv.Atoi(ppidText)
		elapsed, elapsedOK := parseElapsed(elapsedText)
		if pidErr != nil || ppidErr != nil || !elapsedOK || exe == "" {
			continue
		}
		processes = append(processes, unixProcess{pid: pid, ppid: ppid, exe: exe, started: now.Add(-elapsed)})
	}
	return processes
}

// parseElapsed reads ps etime: [[dd-]hh:]mm:ss.
func parseElapsed(text string) (time.Duration, bool) {
	days := 0
	if before, after, ok := strings.Cut(text, "-"); ok {
		value, err := strconv.Atoi(before)
		if err != nil {
			return 0, false
		}
		days, text = value, after
	}
	parts := strings.Split(text, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	seconds := 0
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return 0, false
		}
		seconds = seconds*60 + value
	}
	return time.Duration(days)*24*time.Hour + time.Duration(seconds)*time.Second, true
}

func cutField(line string) (string, string) {
	line = strings.TrimLeft(line, " \t")
	index := strings.IndexAny(line, " \t")
	if index < 0 {
		return line, ""
	}
	return line[:index], strings.TrimLeft(line[index:], " \t")
}
