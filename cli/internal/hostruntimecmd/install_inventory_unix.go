//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// The install inventory runs after every install, upgrade and rollback, also
// after one that failed midway. It is the uninstall inventory (unixSweep) with
// one difference: the version now active, and only that version, stays. It
// lists every Paperboat process, unit, generation file, pin and release slot of
// this account plus the shared machine guard. It stops and removes what belongs
// to another version, then re-scans and names whatever is still there.

// inventoryDrainCap bounds how long a previous generation may keep serving the
// streams it held at activation. It is the longest drain any held stream has.
const inventoryDrainCap = runtimewire.DrainLimit

// inventoryPaths are the generation-specific locations of one account, as
// canonical host paths.
type inventoryPaths struct {
	pins string
	// generationGlobs match per-generation descriptors, locks and endpoints.
	generationGlobs []string
	guardBinaries   []string
}

func newInventoryPaths(plan unixPurgePlan) (inventoryPaths, error) {
	layout, err := service.UserLayout(plan.platform, plan.uid)
	if err != nil {
		return inventoryPaths{}, err
	}
	carrier := filepath.Join(layout.InstallRoot, "carrier")
	p := inventoryPaths{
		pins: filepath.Join(layout.ReleasesRoot, "pins"),
		generationGlobs: []string{
			filepath.Join(carrier, "generation-*.json"),
			filepath.Join(carrier, "owner-*.lock"),
			layout.HostdSocket + ".c*",
			filepath.Join(layout.InstallRoot, "session-owners", "*.json"),
		},
	}
	if plan.platform == "darwin" {
		p.generationGlobs = append(p.generationGlobs, "/Library/Application Support/Paperboat/ipc/"+plan.instance+"-*")
		p.guardBinaries = []string{"/Library/PrivilegedHelperTools/io.paperboat.machineguard"}
	} else {
		p.generationGlobs = append(p.generationGlobs, "/run/paperboat-session-owner-"+plan.instance+"-*")
		p.guardBinaries = []string{"/usr/local/libexec/paperboat-machineguard/pb"}
	}
	return p, nil
}

const noteOutlivedCap = " outlived its drain cap"

// inventoryFinding is one item of another version: a path or a process.
type inventoryFinding struct {
	path    string
	unit    string // the native unit a definition declares, if any
	process *unixProcess
	note    string
	// reportOnly items are named but never removed: an unfinished installation's journal is what the
	// next pb install's recovery restores the exact baseline from.
	reportOnly bool
}

func (f inventoryFinding) String() string {
	if f.process != nil {
		return fmt.Sprintf("process %d (%s)%s", f.process.pid, f.process.exe, f.note)
	}
	return f.path + f.note
}

// settleInventory removes what another version left and fails naming what
// remains. check reports without removing anything (the device harness).
func (s unixSweep) settleInventory(ctx context.Context, check bool) error {
	sources, err := s.sources(ctx)
	if err != nil {
		return withPurgeStage("final_inventory", err)
	}
	findings, err := s.versionLeftovers(ctx, sources)
	if err != nil {
		return withPurgeStage("final_inventory", err)
	}
	// A generation that outlived its drain cap is a failure even once removed.
	var items []string
	for _, finding := range findings {
		if finding.note == noteOutlivedCap {
			items = append(items, finding.String())
		}
	}
	if !check && len(findings) > 0 {
		s.removeFindings(ctx, findings)
		// An install that finished meanwhile removed its journal: read the sources again, never name a gone journal.
		if sources, err = s.sources(ctx); err != nil {
			return withPurgeStage("final_inventory", err)
		}
		if findings, err = s.versionLeftovers(ctx, sources); err != nil {
			return withPurgeStage("final_inventory", err)
		}
		// A draining generation owned its files in the first scan and exited before the second: its
		// definitions are plain leftovers now, with no process left to stop. Remove them once more.
		if files := fileFindings(findings); len(files) > 0 {
			s.removeFindings(ctx, files)
			if findings, err = s.versionLeftovers(ctx, sources); err != nil {
				return withPurgeStage("final_inventory", err)
			}
		}
	}
	for _, finding := range findings {
		if item := finding.String(); !containsString(items, item) {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil
	}
	return withPurgeStage("final_inventory", unixLeftoverError{items: items})
}

// fileFindings are the findings that name a path no process holds.
func fileFindings(findings []inventoryFinding) []inventoryFinding {
	var files []inventoryFinding
	for _, finding := range findings {
		if finding.process == nil && !finding.reportOnly {
			files = append(files, finding)
		}
	}
	return files
}

func (s unixSweep) removeFindings(ctx context.Context, findings []inventoryFinding) {
	var units []string
	var processes []unixProcess
	for _, finding := range findings {
		switch {
		case finding.unit != "" && !containsString(units, finding.unit):
			units = append(units, finding.unit)
		case finding.unit != "":
		case finding.process != nil:
			processes = append(processes, *finding.process)
		}
	}
	// The service manager stops every process of a unit first; signals reach
	// only what no unit owns any more. Removal errors surface in the re-scan.
	s.stopUnits(ctx, units)
	for _, finding := range findings {
		if finding.path != "" && !finding.reportOnly {
			_ = s.removePath(finding.path)
		}
	}
	if len(units) > 0 {
		_ = s.reloadUnits(ctx, units)
	}
	_ = s.stopTargets(ctx, processes)
	// A fixed role that ran a replaced image comes back on the current one.
	// launchd's KeepAlive and the user unit's Restart=always do that already;
	// a system unit with Restart=on-failure (the guard) needs a start.
	var restart []string
	for _, process := range processes {
		unit := process.unit
		if s.plan.platform == "linux" && process.stale && strings.HasPrefix(unit, "paperboat") &&
			generationToken(unit) == "" && unit != "paperboatd.service" && !containsString(restart, unit) {
			restart = append(restart, unit)
		}
	}
	for _, unit := range restart {
		if err := s.run(ctx, "/usr/bin/systemctl", "start", unit); err != nil {
			observeRuntimeFailure(ctx, "install", runtimeFailure("restarting a replaced role failed", err))
		}
	}
}

// versionLeftovers lists every item of this account and the shared guard that
// the active version does not own. Owned: the active version; session-owner
// generations its catalogue references (they hold PTYs across updates); and a
// previous generation's processes that started before the activation, until the
// drain cap. With an unfinished installation journal only processes running a
// replaced image are leftovers: the next install recovers the files.
func (s unixSweep) versionLeftovers(ctx context.Context, sources hostinstall.InventorySources) ([]inventoryFinding, error) {
	paths, err := newInventoryPaths(s.plan)
	if err != nil {
		return nil, err
	}
	processes, err := s.inventoryProcesses(ctx, paths)
	if err != nil {
		return nil, err
	}
	// owned digests keep every item of their generation; retained ones keep
	// only their session owner, which holds PTYs across updates.
	owned := map[string]bool{}
	if sources.Active != "" {
		owned[sources.Active] = true
	}
	// referenced holds release slots (not pins) a live owned process executes; nothing else under
	// releases/ but pins/ may stay.
	referenced := map[string]bool{}
	releases := filepath.Dir(paths.pins)
	var findings []inventoryFinding
	now := s.now()
	for index := range processes {
		process := &processes[index]
		digest, pinned := pinDigest(paths.pins, process.exe)
		slot := ""
		if !pinned && strings.HasPrefix(process.exe, releases+"/") {
			slot = filepath.Join(releases, strings.SplitN(strings.TrimPrefix(process.exe, releases+"/"), "/", 2)[0])
		}
		switch {
		case process.stale:
			findings = append(findings, inventoryFinding{process: process, note: " runs a replaced image"})
		case !pinned && slot == "":
			// bin/pb or the guard, with its current image: the active version
		case pinned && owned[digest]:
		case pinned && retainedOwner(sources, digest) && sessionOwnerProcess(*process):
		case sources.Pending != "":
			// a transaction in progress runs its candidate and slots; the next inventory decides
			if slot != "" {
				referenced[slot] = true
			}
		case process.started.Before(sources.Activated) && now.Before(sources.Activated.Add(inventoryDrainCap)):
			// a draining previous generation keeps the binary it runs from, and its generation's files
			if pinned {
				owned[digest] = true
			} else {
				referenced[slot] = true
			}
		case process.started.Before(sources.Activated):
			findings = append(findings, inventoryFinding{process: process, note: noteOutlivedCap})
		default:
			findings = append(findings, inventoryFinding{process: process})
		}
	}
	if sources.Pending != "" {
		return append(findings, inventoryFinding{path: sources.Pending, note: " (installation not finished; rerun pb install)", reportOnly: true}), nil
	}
	retained := map[string]bool{}
	for digest := range sources.Retained {
		if retainedOwner(sources, digest) {
			retained[digest] = true
		}
	}
	files, err := s.versionFiles(paths, owned, retained, referenced)
	return append(findings, files...), err
}

// retainedOwner reports a catalogue generation whose session owner may stay:
// one published before the active slot was activated. A descriptor written
// after it belongs to an update that never committed (pbvm1: each failed
// update left its candidate's session owner running), not to history.
func retainedOwner(sources hostinstall.InventorySources, digest string) bool {
	written, ok := sources.Retained[digest]
	return ok && !written.After(sources.Activated)
}

// sessionOwnerProcess reports a session owner by its unit or job label.
func sessionOwnerProcess(process unixProcess) bool {
	return strings.Contains(process.unit, "session-owner")
}

// versionFiles lists definitions, generation files, pins and release slots of
// generations that no owned digest names.
func (s unixSweep) versionFiles(paths inventoryPaths, owned, retained, referenced map[string]bool) ([]inventoryFinding, error) {
	var findings []inventoryFinding
	ownedName := func(path string) bool {
		name := filepath.Base(path)
		token := generationToken(name)
		if token == "" {
			return true // a fixed role of the account, not a generation
		}
		sessionOwner := strings.Contains(name, "session-owner") || strings.HasPrefix(name, s.plan.instance+"-") ||
			filepath.Base(filepath.Dir(path)) == "session-owners"
		for digest := range owned {
			if strings.HasPrefix(digest, token) {
				return true
			}
		}
		for digest := range retained {
			if sessionOwner && strings.HasPrefix(digest, token) {
				return true
			}
		}
		return false
	}
	definitions, err := s.matchedDefinitions()
	for _, definition := range definitions {
		if !ownedName(definition) {
			findings = append(findings, inventoryFinding{path: definition, unit: unitName(s.plan.platform, definition)})
		}
	}
	// These globs sit in the account's own directories or carry its token
	// followed by '-', so no other account matches them.
	var generationFiles []string
	for _, pattern := range paths.generationGlobs {
		matches, globErr := filepath.Glob(s.at(pattern))
		err = errors.Join(err, globErr)
		for _, match := range matches {
			generationFiles = append(generationFiles, strings.TrimPrefix(match, s.root))
		}
	}
	for _, path := range generationFiles {
		if !ownedName(path) {
			findings = append(findings, inventoryFinding{path: path})
		}
	}
	pins, globErr := filepath.Glob(s.at(filepath.Join(paths.pins, "*")))
	err = errors.Join(err, globErr)
	for _, pin := range pins {
		if name := filepath.Base(pin); !owned[name] && !retained[name] {
			findings = append(findings, inventoryFinding{path: strings.TrimPrefix(pin, s.root)})
		}
	}
	// releases/ holds pins/ (each pin judged above by its digest) and the slots of a transaction. With no
	// transaction pending a slot is removed only when nothing references it: no live owned process (the
	// current generation or one draining within its cap) executes it. Listing the directory, not today's
	// slot names, also finds what an older build left there (pb.previous).
	slots, globErr := filepath.Glob(s.at(filepath.Join(filepath.Dir(paths.pins), "*")))
	err = errors.Join(err, globErr)
	for _, slot := range slots {
		if slot = strings.TrimPrefix(slot, s.root); slot != paths.pins && !referenced[slot] {
			findings = append(findings, inventoryFinding{path: slot})
		}
	}
	return findings, err
}

// inventoryProcesses returns the owned main processes of this account and of
// the shared guard (ownedProcesses), each with whether it runs a replaced image.
func (s unixSweep) inventoryProcesses(ctx context.Context, paths inventoryPaths) ([]unixProcess, error) {
	processes, err := s.ownedProcesses(ctx, append(append([]string(nil), s.plan.payloadPaths...), paths.guardBinaries...))
	if err != nil {
		return nil, err
	}
	for index := range processes {
		if processes[index].stale, err = s.replacedImage(ctx, processes[index]); err != nil {
			return nil, err
		}
	}
	return processes, nil
}

// ourUnit reports a unit or job this account's install owns: one carrying its
// exact token, the shared guard's, or the account's own user daemon.
func (s unixSweep) ourUnit(name string) bool {
	switch name {
	case "paperboat-machineguard.service", "io.paperboat.machineguard", "paperboatd.service", service.DaemonLabel:
		return true
	}
	return (strings.HasPrefix(name, "paperboat-") || strings.HasPrefix(name, "com.pinksaucepasta.paperboat.")) && ownsInstance(name, s.plan.instance)
}

// replacedImage reports a process running an image other than the file now at
// its path. An in-place exec handover keeps the pid and start time but maps the
// new file, so it compares images, not times.
func (s unixSweep) replacedImage(ctx context.Context, process unixProcess) (bool, error) {
	if s.imageInodes == nil {
		return process.stale, nil // Linux: the listing compared /proc/<pid>/exe
	}
	info, err := os.Stat(s.at(process.exe))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("installed executable has no inode")
	}
	inodes, err := s.imageInodes(ctx, process.pid)
	if err != nil {
		return false, err
	}
	for _, inode := range inodes {
		if inode == uint64(stat.Ino) {
			return false, nil
		}
	}
	return len(inodes) > 0, nil
}

// pinDigest returns the source digest of a pinned executable path.
func pinDigest(pins, exe string) (string, bool) {
	relative, ok := strings.CutPrefix(exe, pins+"/")
	if !ok {
		return "", false
	}
	digest, rest, _ := strings.Cut(relative, "/")
	return digest, rest == "pb" && len(digest) == 64
}

// generationToken returns the generation a name carries: 32 hex (units,
// descriptors, locks, endpoints) or a carrier socket's "c" + 16 hex.
func generationToken(name string) string {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '.' }) {
		if len(part) == 17 && part[0] == 'c' {
			part = part[1:]
		}
		if (len(part) == 32 || len(part) == 16) && strings.ToLower(part) == part {
			if _, err := hex.DecodeString(part); err == nil {
				return part
			}
		}
	}
	return ""
}

// unitName is the systemd unit or launchd label a definition path declares.
func unitName(platform, definition string) string {
	name := filepath.Base(definition)
	if platform == "linux" {
		return strings.TrimSuffix(name, ".d")
	}
	return strings.TrimSuffix(name, ".plist")
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (s unixSweep) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// runInstallInventory is the root side of `__runtime-service inventory` and of a root install
// operation's end. It holds the native installation lock, so it never runs while an update or install
// stages and executes release slots (the user-side inventory otherwise could race an updater activation).
func runInstallInventory(ctx context.Context, uid int, check bool) error {
	layout, err := service.UserLayout(runtime.GOOS, uid)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(layout.UpdateStateRoot); statErr == nil {
		lock, err := updated.LockUnixNativeInstall(ctx, layout.UpdateStateRoot)
		if err != nil {
			return withPurgeStage("final_inventory", err)
		}
		defer lock.Close()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return withPurgeStage("final_inventory", statErr)
	}
	return runInstallInventoryLocked(ctx, uid, check)
}

// runInstallInventoryLocked is the inventory for a caller that already holds the installation lock.
func runInstallInventoryLocked(ctx context.Context, uid int, check bool) error {
	plan, err := newUnixPurgePlan(runtime.GOOS, uid)
	if err != nil {
		return err
	}
	return newHostUnixSweep(plan).settleInventory(ctx, check)
}

// InventoryUnsupportedError is an executable whose privileged service has no install inventory: a
// build older than the pb driving the install (a downgrade). Version is empty when it cannot be read.
type InventoryUnsupportedError struct{ Version string }

func (e InventoryUnsupportedError) Error() string {
	name := "this Paperboat build"
	if e.Version != "" {
		name = "Paperboat " + e.Version
	}
	return name + " cannot run the install inventory (it is older than this pb); install it with its own installer"
}

// inspectInventoryExecutable reads the version of the executable that ran the inventory.
var inspectInventoryExecutable = installsource.InspectCandidate

// runInventoryCommand runs the privileged inventory with executable and turns what it could not remove
// into a failure that names each item.
func runInventoryCommand(ctx context.Context, command *exec.Cmd, executable string) error {
	var output purgeOutput
	command.Stdout = &output
	command.Stderr = os.Stderr
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == ServiceUsageExitCode {
		unsupported := InventoryUnsupportedError{}
		if source, inspectErr := inspectInventoryExecutable(ctx, executable); inspectErr == nil {
			unsupported.Version = source.Version
		}
		return runtimeFailure(unsupported.Error(), unsupported, err)
	}
	if output.exceeded {
		return runtimeFailure("the install inventory result exceeded its bound", err)
	}
	err = readPurgeResult(output.data, errors.Join(err, runtimeContextFailure(ctx)))
	var incomplete PurgeIncomplete
	if errors.As(err, &incomplete) && len(incomplete.Remaining) > 0 {
		return runtimeFailure("items of another Paperboat version are still on this machine and could not be removed: "+strings.Join(incomplete.Remaining, ", ")+"; run pb doctor, then pb install again", err)
	}
	return err
}
