//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

func runPurgeCommand(ctx context.Context, args []string, _ io.Reader, _, _ io.Writer) error {
	if len(args) != 0 && (len(args) != 1 || args[0] != "--preserve-cli") {
		return errors.New("purge accepts no arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	arguments := append([]string{"--", "/usr/bin/env", "PAPERBOAT_INVOKING_UID=" + strconv.Itoa(os.Getuid()), executable, "__runtime-service", "purge"}, args...)
	command := exec.CommandContext(ctx, "/usr/bin/sudo", arguments...)
	command.Stderr = io.Discard
	var output purgeOutput
	command.Stdout = &output
	err = command.Run()
	if output.exceeded {
		return runtimeFailure("privileged cleanup result exceeded its bound", err)
	}
	return readPurgeResult(output.data, errors.Join(err, runtimeContextFailure(ctx)))
}

// purgeSystemInstallation is the root side of uninstall and reset. The order is
// the contract: native uninstall of this account when its metadata exists, an
// inventory sweep for this account, the machine guard only when no account
// remains installed, and finally a re-scan that names every leftover.
func purgeSystemInstallation(ctx context.Context) error {
	return purgeSystemInstallationScoped(ctx, false)
}

func purgeSystemInstallationScoped(ctx context.Context, preserveCLI bool) error {
	if os.Geteuid() != 0 {
		return errors.New("complete uninstall requires administrator approval")
	}
	plan, err := newUnixPurgePlan(runtime.GOOS, invokingServiceUID())
	if err != nil {
		return err
	}
	sweep := newHostUnixSweep(plan)
	if preserveCLI {
		keep, err := hostinstall.ResetCLIForCurrentAccount()
		if err != nil {
			return err
		}
		sweep.keepCLI = keep
		sweep.uninstall = hostinstall.UninstallPersistedPreservingCLI
	}
	return sweep.purgeAccount(ctx)
}

// unixPurgePlan is the Paperboat-owned inventory for one enrolled account on a
// Unix host. Fixed paths are listed explicitly; generation-specific items
// (session owners, carriers, activators, drop-ins, links) are matched by glob
// and must carry this account's exact token.
type unixPurgePlan struct {
	platform string
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
				"/run/paperboat-*-" + instance + "*",
				"/var/run/paperboat-*-" + instance + "*",
				"/var/cache/paperboat*-" + instance + "*",
			},
			accountRecords: "/var/lib/paperboat-installer/users",
		}, nil
	case "darwin":
		labels := []string{service.HostLabel + "." + instance, service.HostdLabel + "." + instance, service.UpdaterLabel + "." + instance}
		definitions := make([]string, 0, len(labels))
		for _, label := range labels {
			definitions = append(definitions, filepath.Join("/Library", "LaunchDaemons", label+".plist"))
		}
		return unixPurgePlan{
			platform:    platform,
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
				"/var/run/paperboat-hostd-" + instance,
				"/var/run/paperboat-updated-" + instance,
			},
			payloadGlobs: []string{
				"/var/run/paperboat-*-" + instance + "*",
				"/Library/Application Support/Paperboat/ipc/" + instance + "-*",
				"/private/var/log/com.pinksaucepasta.paperboat.*." + instance + "*.log",
			},
			accountRecords: "/Library/Application Support/Paperboat/users",
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
}

// unixSweep performs one account's teardown. Paths in the plan are canonical
// host paths; root prefixes every filesystem operation so tests run in a temp
// tree. Every external effect is injected.
type unixSweep struct {
	root          string
	keepCLI       string
	plan          unixPurgePlan
	run           func(context.Context, string, ...string) error
	remove        func(string) error
	processes     func(context.Context) ([]unixProcess, error)
	signal        func(pid int, sig syscall.Signal) error
	uninstall     func(context.Context) error
	guard         func(context.Context) error
	terminateWait time.Duration
	killWait      time.Duration
}

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
		uninstall:     hostinstall.UninstallPersisted,
		guard:         machineguard.Purge,
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
	if metadataPresent {
		if err := s.uninstall(ctx); err != nil {
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
		if s.keepCLI == "" && result == nil {
			result = errors.Join(result, withPurgeStage("shared_policy", s.removeLegacyAvailabilityPolicy(ctx)))
		}
	}
	if s.plan.platform == "darwin" && s.keepCLI == "" {
		if err := s.removeUnneededSharedManuals(); err != nil {
			result = errors.Join(result, withPurgeStage("shared_manuals", err))
		}
	}
	if s.keepCLI == "" {
		result = errors.Join(result, withPurgeStage("shared_payload_parents", s.removeEmptySharedPayloadParents(ctx)))
	}

	leftovers, err := s.leftovers(ctx)
	result = errors.Join(result, withPurgeStage("final_inventory", err))
	if len(leftovers) > 0 {
		result = errors.Join(result, withPurgeStage("final_inventory", unixLeftoverError{items: leftovers}))
	}
	return runtimeFailureIfAny("removing the Paperboat installation failed", result)
}

func (s unixSweep) removeLegacyAvailabilityPolicy(ctx context.Context) (resultErr error) {
	root := "/var/lib/paperboat"
	if s.plan.platform == "darwin" {
		root = "/Library/Application Support/Paperboat"
	}
	path := s.at(filepath.Join(root, "availability-policy.json"))
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	boundary := s.at("/")
	relative, err := filepath.Rel(boundary, filepath.Dir(path))
	if err != nil {
		return err
	}
	current := boundary
	for _, part := range append([]string{""}, strings.Split(relative, string(filepath.Separator))...) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("legacy availability directory custody is unsafe")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || info.Size() > 16<<10 {
		return errors.New("legacy availability policy custody is unsafe")
	}
	decoder := json.NewDecoder(io.LimitReader(file, (16<<10)+1))
	decoder.DisallowUnknownFields()
	var state hostservice.State
	var extra any
	if decoder.Decode(&state) != nil || decoder.Decode(&extra) != io.EOF || state.Schema != hostservice.ProtocolV1 {
		return errors.New("legacy availability policy is not owned")
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, named) {
		return errors.New("legacy availability policy changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
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
	// Check every parent without creating or repairing shared filesystem paths.
	// The root prefix is the existing sweep's isolated filesystem fixture.
	current := s.at("/")
	for _, component := range []string{"", "usr", "local", "share", "man", "man1"} {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || !ok || stat.Uid != 0 {
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
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
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
	if s.plan.platform == "linux" {
		for _, unit := range units {
			for _, action := range []string{"stop", "disable"} {
				// A unit that never loaded or is already gone reports an error;
				// absence is confirmed by the file removal and re-scan below.
				if err := s.run(ctx, "/usr/bin/systemctl", action, unit); err != nil {
					observeRuntimeFailure(ctx, "uninstall", runtimeFailure("service cleanup could not be confirmed", err))
				}
			}
		}
	} else {
		for _, label := range units {
			if err := s.run(ctx, "/bin/launchctl", "bootout", "system/"+label); err != nil {
				observeRuntimeFailure(ctx, "uninstall", runtimeFailure("service cleanup could not be confirmed", err))
			}
		}
	}
	for _, path := range append(append([]string(nil), s.plan.definitions...), definitions...) {
		result = errors.Join(result, s.removePath(path))
	}
	if s.plan.platform == "linux" {
		result = errors.Join(result, s.run(ctx, "/usr/bin/systemctl", "daemon-reload"))
		arguments := append([]string{"reset-failed"}, units...)
		if err := s.run(ctx, "/usr/bin/systemctl", arguments...); err != nil {
			observeRuntimeFailure(ctx, "uninstall", runtimeFailure("service cleanup could not be confirmed", err))
		}
	}
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

// accountProcesses returns processes whose executable lies under this
// account's payload roots, excluding this process and its ancestors. The
// ancestors include the user-side pb that launched this purge.
func (s unixSweep) accountProcesses(ctx context.Context) ([]unixProcess, error) {
	processes, err := s.processes(ctx)
	if err != nil {
		return nil, err
	}
	excluded := processTreeOf(processes, os.Getpid())
	var matches []unixProcess
	for _, process := range processes {
		if excluded[process.pid] || !underAnyRoot(process.exe, s.plan.payloadPaths) {
			continue
		}
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
		current, err := s.accountProcesses(ctx)
		if err != nil {
			return nil, err
		}
		var alive []unixProcess
		for _, target := range targets {
			for _, process := range current {
				if process.pid == target.pid {
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
		if present {
			items = append(items, path)
		}
	}
	processes, err := s.accountProcesses(ctx)
	failure = errors.Join(failure, err)
	for _, process := range processes {
		items = append(items, fmt.Sprintf("process %d (%s)", process.pid, process.exe))
	}
	return items, failure
}

func listLinuxProcesses(context.Context) ([]unixProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []unixProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if err != nil {
			// The process exited or is a kernel thread; neither owns payload.
			continue
		}
		// A replaced binary keeps running as "<path> (deleted)".
		exe = strings.TrimSuffix(exe, " (deleted)")
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		processes = append(processes, unixProcess{pid: pid, ppid: linuxParentPID(stat), exe: exe})
	}
	return processes, nil
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
// which may contain spaces, so only the first two columns are split off.
func listDarwinProcesses(ctx context.Context) ([]unixProcess, error) {
	output, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, err
	}
	return parseDarwinProcesses(string(output)), nil
}

func parseDarwinProcesses(output string) []unixProcess {
	var processes []unixProcess
	for _, line := range strings.Split(output, "\n") {
		pidText, rest := cutField(line)
		ppidText, exe := cutField(rest)
		pid, pidErr := strconv.Atoi(pidText)
		ppid, ppidErr := strconv.Atoi(ppidText)
		if pidErr != nil || ppidErr != nil || exe == "" {
			continue
		}
		processes = append(processes, unixProcess{pid: pid, ppid: ppid, exe: exe})
	}
	return processes
}

func cutField(line string) (string, string) {
	line = strings.TrimLeft(line, " \t")
	index := strings.IndexAny(line, " \t")
	if index < 0 {
		return line, ""
	}
	return line[:index], strings.TrimLeft(line[index:], " \t")
}
