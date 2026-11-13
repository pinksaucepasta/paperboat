//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

// One account's host as the inventory sees it, for two versions A and B.
var (
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	digestC = strings.Repeat("c", 64)
)

const (
	invRoot  = "/usr/local/libexec/paperboat/users/u1001"
	invGuard = "/usr/local/libexec/paperboat-machineguard/pb"
)

func invPin(digest string) string { return invRoot + "/releases/pins/" + digest + "/pb" }

// versionFiles are what one installed generation of u1001 owns on Linux.
func versionFiles(digest string) []string {
	generation := digest[:32]
	return []string{
		invPin(digest),
		"/etc/systemd/system/paperboat-hostd-u1001-" + generation + ".service",
		"/etc/systemd/system/paperboat-carrier-u1001-" + generation + ".service",
		"/etc/systemd/system/multi-user.target.wants/paperboat-hostd-u1001-" + generation + ".service",
		invRoot + "/carrier/generation-" + generation + ".json",
		invRoot + "/carrier/owner-" + generation + ".lock",
		"/run/paperboat-hostd-u1001/hostd.sock.c" + digest[:16],
	}
}

func sessionOwnerFiles(digest string) []string {
	return []string{
		invRoot + "/session-owners/" + digest[:32] + ".json",
		"/etc/systemd/system/paperboat-session-owner-u1001-" + digest[:32] + ".service",
		"/run/paperboat-session-owner-u1001-" + digest[:32] + "/control.sock",
	}
}

// fixedFiles belong to the account whatever version is active.
var fixedFiles = []string{
	invRoot + "/bin/pb",
	"/etc/systemd/system/paperboat-hostd-u1001.service",
	"/etc/systemd/system/paperboat-updated-u1001.service",
	invGuard,
}

type inventoryHost struct {
	t       *testing.T
	root    string
	fake    *purgeFake
	sweep   unixSweep
	sources hostinstall.InventorySources
}

func newInventoryHost(t *testing.T, active string, activated time.Time, now time.Time, files []string, processes ...unixProcess) *inventoryHost {
	t.Helper()
	root := t.TempDir()
	for _, path := range append(append([]string(nil), fixedFiles...), files...) {
		plantFile(t, root, path)
	}
	plan, err := newUnixPurgePlan("linux", 1001)
	if err != nil {
		t.Fatal(err)
	}
	h := &inventoryHost{t: t, root: root, fake: newPurgeFake(processes...)}
	h.sources = hostinstall.InventorySources{Active: active, Activated: activated, Retained: map[string]time.Time{}}
	h.sweep = testSweep(root, plan, h.fake)
	h.sweep.sources = func(context.Context) (hostinstall.InventorySources, error) { return h.sources, nil }
	jobs := map[int]string{}
	for _, process := range processes {
		if process.ppid == 1 && process.unit != "" {
			jobs[process.pid] = process.unit
		}
	}
	h.sweep.jobs = func(context.Context) (map[int]string, error) { return jobs, nil }
	h.sweep.clock = func() time.Time { return now }
	return h
}

// assertExactlyActive is the per-path contract: the re-scan finds nothing, the
// given items remain, and every other planted item is gone.
func (h *inventoryHost) assertExactlyActive(keep []string, gone []string, alive ...int) {
	h.t.Helper()
	findings, err := h.sweep.versionLeftovers(context.Background(), h.sources)
	if err != nil || len(findings) != 0 {
		h.t.Fatalf("inventory after settle = %v, %v; want only the active version", findings, err)
	}
	assertPresent(h.t, h.root, append(append([]string(nil), fixedFiles...), keep...)...)
	assertGone(h.t, h.root, gone...)
	var pids []int
	for pid := range h.fake.alive {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	if len(alive) == 0 {
		alive = nil
	}
	if !reflect.DeepEqual(pids, alive) {
		h.t.Fatalf("alive processes = %v, want %v (signals %v)", pids, alive, h.fake.signals)
	}
}

func managedProcess(pid int, exe, unit string, started time.Time) unixProcess {
	return unixProcess{pid: pid, ppid: 1, exe: exe, started: started, unit: unit}
}

func TestInstallInventoryFreshInstallKeepsEverythingOfTheActiveVersion(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	files := append(versionFiles(digestA), sessionOwnerFiles(digestA)...)
	h := newInventoryHost(t, digestA, activated, activated.Add(time.Minute), files,
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second)),
		managedProcess(4002, invRoot+"/bin/pb", "paperboat-updated-u1001.service", activated.Add(time.Second)),
		managedProcess(4003, invGuard, "paperboat-machineguard.service", activated.Add(time.Second)),
		// An interactive pb in a login session is not the install's.
		unixProcess{pid: 4004, ppid: 900, exe: invRoot + "/bin/pb", stale: true, unit: "session-3.scope"},
	)
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(files, nil, 4001, 4002, 4003, 4004)
	if len(h.fake.commands) != 0 || len(h.fake.signals) != 0 {
		t.Fatalf("a clean install was changed: commands=%v signals=%v", h.fake.commands, h.fake.signals)
	}
}

// Seen on the Mac: a failed two-user upgrade left the old machine guard running.
// Every item of the old version goes: its units by the service manager, its
// processes by signal, its pin, descriptors, locks, sockets and release slots.
func TestInstallInventoryUpgradeRemovesThePreviousVersion(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	old := append(versionFiles(digestA), invRoot+"/releases/pb.rollback", invRoot+"/releases/pb.staged")
	files := append(append([]string(nil), versionFiles(digestB)...), old...)
	h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), files,
		managedProcess(4001, invPin(digestB), "paperboat-hostd-u1001-"+digestB[:32]+".service", activated.Add(time.Second)),
		// The old guard and the old updater still run images the upgrade replaced.
		unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, started: activated.Add(-time.Hour), unit: "paperboat-machineguard.service"},
		unixProcess{pid: 4003, ppid: 1, exe: invRoot + "/bin/pb", stale: true, started: activated.Add(-time.Hour), unit: "paperboatd.service"},
		managedProcess(4004, invGuard, "paperboat-machineguard.service", activated.Add(time.Second)),
	)
	h.fake.termExits[4002], h.fake.termExits[4003] = true, true
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(versionFiles(digestB), old, 4001, 4004)
	// The guard's unit restarts only on failure: the inventory starts it again on the current image.
	if !containsString(h.fake.commands, "/usr/bin/systemctl start paperboat-machineguard.service") || containsString(h.fake.commands, "/usr/bin/systemctl start paperboatd.service") {
		t.Fatalf("replaced fixed roles not restarted exactly once each: %v", h.fake.commands)
	}
	for _, unit := range []string{"paperboat-hostd-u1001-" + digestA[:32] + ".service", "paperboat-carrier-u1001-" + digestA[:32] + ".service"} {
		if !containsString(h.fake.commands, "/usr/bin/systemctl stop "+unit) {
			t.Fatalf("old unit %s not stopped by systemd: %v", unit, h.fake.commands)
		}
	}
}

// An upgrade that failed midway was restored to A: the candidate B started
// after A's activation, so it is not a draining generation and goes.
func TestInstallInventoryFailedMidwayUpgradeRemovesTheCandidate(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	candidate := append(versionFiles(digestB), invRoot+"/releases/pb.staged")
	h := newInventoryHost(t, digestA, activated, activated.Add(time.Hour), append(append([]string(nil), versionFiles(digestA)...), candidate...),
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Minute)),
		managedProcess(4002, invPin(digestB), "paperboat-carrier-u1001-"+digestB[:32]+".service", activated.Add(30*time.Minute)),
	)
	h.fake.termExits[4002] = true
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(versionFiles(digestA), candidate, 4001)
}

// A failure that leaves the installation journal: the next pb install recovers
// its files, so only replaced images go and the inventory fails naming the journal.
func TestInstallInventoryUnfinishedInstallationFailsLoudly(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	// The journal is planted: the next pb install's recovery restores the exact baseline from it, so the
	// inventory names it and never removes it.
	journal := "/var/lib/paperboat-installer/users/u1001/install-journal.json"
	files := append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...)
	files = append(files, journal)
	h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), files,
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second)),
		unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, unit: "paperboat-machineguard.service"},
	)
	h.fake.termExits[4002] = true
	h.sources.Pending = journal
	err := h.sweep.settleInventory(context.Background(), false)
	var leftovers unixLeftoverError
	if !errors.As(err, &leftovers) || len(leftovers.items) != 1 || !strings.HasPrefix(leftovers.items[0], journal) {
		t.Fatalf("err = %v, want only the unfinished journal named", err)
	}
	assertPresent(t, h.root, files...)
	if _, alive := h.fake.alive[4002]; alive {
		t.Fatal("the guard running a replaced image survived")
	}
}

// An install that commits while the inventory runs removes its journal. The inventory must read the journal again
// after its removal pass and must not name one that is gone: a successful install exits 0.
func TestInstallInventoryDoesNotNameAJournalThatWasCommittedMeanwhile(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	journal := "/var/lib/paperboat-installer/users/u1001/install-journal.json"
	files := append([]string(nil), versionFiles(digestB)...)
	h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), files,
		unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, unit: "paperboat-machineguard.service"},
	)
	h.fake.termExits[4002] = true
	reads := 0
	h.sweep.sources = func(context.Context) (hostinstall.InventorySources, error) {
		reads++
		sources := h.sources
		if reads == 1 {
			sources.Pending = journal // the journal exists when the inventory starts, the commit removes it before the next read
		}
		return sources, nil
	}
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatalf("err = %v, want a clean inventory once the journal is gone", err)
	}
	if reads < 2 {
		t.Fatalf("sources read %d times, want a fresh read after the removal pass", reads)
	}
}

// Rolling back from B to A restores A's slot (and its old activation time):
// B's processes, units and files are leftovers; A's session owner and the
// generations its catalogue still references stay.
func TestInstallInventoryRollbackLeavesNoNewVersionItems(t *testing.T) {
	activatedA := time.Unix(1_800_000_000, 0)
	newer := append(append([]string(nil), versionFiles(digestB)...), sessionOwnerFiles(digestB)...)
	newer = append(newer, invRoot+"/releases/pb.rollback")
	keep := append(append(append([]string(nil), versionFiles(digestA)...), sessionOwnerFiles(digestA)...), sessionOwnerFiles(digestC)...)
	keep = append(keep, invPin(digestC))
	h := newInventoryHost(t, digestA, activatedA, activatedA.Add(48*time.Hour), append(append([]string(nil), keep...), newer...),
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activatedA.Add(47*time.Hour)),
		managedProcess(4002, invPin(digestB), "paperboat-hostd-u1001-"+digestB[:32]+".service", activatedA.Add(24*time.Hour)),
		managedProcess(4003, invPin(digestB), "paperboat-session-owner-u1001-"+digestB[:32]+".service", activatedA.Add(24*time.Hour)),
		// An older session owner the catalogue still references holds live PTYs.
		managedProcess(4004, invPin(digestC), "paperboat-session-owner-u1001-"+digestC[:32]+".service", activatedA.Add(-72*time.Hour)),
		// A carrier of that same old version is no session owner; it started after A came back.
		managedProcess(4005, invPin(digestC), "paperboat-carrier-u1001-"+digestC[:32]+".service", activatedA.Add(24*time.Hour)),
	)
	h.sources.Retained[digestC] = activatedA.Add(-72 * time.Hour)
	for _, pid := range []int{4002, 4003, 4005} {
		h.fake.termExits[pid] = true
	}
	err := h.sweep.settleInventory(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(keep, newer, 4001, 4004)
}

// pbvm1: every update that failed at service apply left its candidate's session owner running, with its
// catalogue descriptor, so the catalogue "retained" it. A descriptor written after the active slot was
// activated belongs to a failed candidate, not to history: it goes, while older retained owners stay.
func TestInstallInventoryRemovesAFailedUpdatesCandidateSessionOwner(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	keep := append(append(append(append([]string(nil), versionFiles(digestA)...), sessionOwnerFiles(digestA)...), sessionOwnerFiles(digestC)...), invPin(digestC))
	candidate := append(sessionOwnerFiles(digestB), invPin(digestB))
	h := newInventoryHost(t, digestA, activated, activated.Add(time.Hour), append(append([]string(nil), keep...), candidate...),
		managedProcess(4001, invPin(digestA), "paperboat-session-owner-u1001-"+digestA[:32]+".service", activated.Add(time.Second)),
		managedProcess(4002, invPin(digestB), "paperboat-session-owner-u1001-"+digestB[:32]+".service", activated.Add(10*time.Minute)),
		managedProcess(4003, invPin(digestC), "paperboat-session-owner-u1001-"+digestC[:32]+".service", activated.Add(-48*time.Hour)),
	)
	h.sources.Retained[digestA] = activated.Add(time.Second)
	h.sources.Retained[digestB] = activated.Add(10 * time.Minute) // the failed update's own descriptor
	h.sources.Retained[digestC] = activated.Add(-48 * time.Hour)  // history from before this version
	h.fake.termExits[4002] = true
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(keep, candidate, 4001, 4003)
	if !containsString(h.fake.commands, "/usr/bin/systemctl stop paperboat-session-owner-u1001-"+digestB[:32]+".service") {
		t.Fatalf("candidate session owner not stopped by its unit: %v", h.fake.commands)
	}
}

// soft-g-11051952: a downgrade to dev-ab41be003, a build older than pb.previous's removal, left that build's
// releases/pb.previous; the install inventory listed only today's slot names and missed it. Every release
// slot other than pins/ is a leftover once no installation is pending, whatever an older build called it.
func TestInstallInventoryRemovesAnyReleaseSlotAnOlderBuildLeft(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	slots := []string{invRoot + "/releases/pb.previous", invRoot + "/releases/pb.some-older-slot"}
	h := newInventoryHost(t, digestA, activated, activated.Add(time.Minute), append(append([]string(nil), versionFiles(digestA)...), slots...),
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second)))
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(versionFiles(digestA), slots, 4001)
}

// What lives under releases/ on a healthy install is pins/<sha>/pb only: the active version's, the
// catalogue's retained session owners' and a draining generation's. All stay, and so does the active
// binary (bin/pb, outside releases/) that the fixed roles run.
func TestInstallInventoryKeepsTheActiveReleaseAndEveryReferencedPin(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	keep := append(append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...), invPin(digestC))
	keep = append(keep, sessionOwnerFiles(digestC)...)
	h := newInventoryHost(t, digestA, activated, activated.Add(time.Hour), keep,
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second)),
		managedProcess(4002, invRoot+"/bin/pb", "paperboat-updated-u1001.service", activated.Add(time.Second)),
		managedProcess(4003, invPin(digestB), "paperboat-carrier-u1001-"+digestB[:32]+".service", activated.Add(-time.Hour)), // draining
		managedProcess(4004, invPin(digestC), "paperboat-session-owner-u1001-"+digestC[:32]+".service", activated.Add(-48*time.Hour)),
	)
	h.sources.Retained[digestC] = activated.Add(-48 * time.Hour)
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(keep, nil, 4001, 4002, 4003, 4004)
}

// A generation an older build started from a release slot (not a pin) keeps that slot while it drains
// within its cap; past the cap the slot and its process go and the inventory fails naming it.
func TestInstallInventoryKeepsASlotADrainingGenerationRunsFrom(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	slot := invRoot + "/releases/pb.previous"
	files := append(append([]string(nil), versionFiles(digestA)...), slot)
	drainer := managedProcess(4002, slot, "paperboat-hostd-u1001.service", activated.Add(-time.Hour))
	active := managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second))

	within := newInventoryHost(t, digestA, activated, activated.Add(inventoryDrainCap-time.Minute), files, active, drainer)
	if err := within.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	within.assertExactlyActive(files, nil, 4001, 4002)

	past := newInventoryHost(t, digestA, activated, activated.Add(inventoryDrainCap+time.Minute), files, active, drainer)
	past.fake.termExits[4002] = true
	err := past.sweep.settleInventory(context.Background(), false)
	var leftovers unixLeftoverError
	if !errors.As(err, &leftovers) || len(leftovers.items) != 1 || !strings.Contains(leftovers.items[0], "process 4002") {
		t.Fatalf("err = %v, want the over-cap generation named", err)
	}
	assertGone(t, past.root, slot)
}

// While a transaction is pending (journal, or a signed update's receipt), its slots and the candidate
// running from them are in use: nothing under releases/ is removed and no candidate is stopped.
func TestInstallInventoryKeepsSlotsDuringAPendingTransaction(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	slots := []string{invRoot + "/releases/pb.staged", invRoot + "/releases/pb.rollback"}
	files := append(append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...), slots...)
	h := newInventoryHost(t, digestA, activated, activated.Add(time.Minute), files,
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second)),
		managedProcess(4002, invRoot+"/releases/pb.staged", "paperboat-updated-u1001.service", activated.Add(30*time.Second)),
		managedProcess(4003, invPin(digestB), "paperboat-carrier-u1001-"+digestB[:32]+".service", activated.Add(30*time.Second)),
	)
	h.sources.Pending = "/var/lib/paperboat-updated-u1001/native-install.json"
	err := h.sweep.settleInventory(context.Background(), false)
	var leftovers unixLeftoverError
	if !errors.As(err, &leftovers) || len(leftovers.items) != 1 || !strings.HasPrefix(leftovers.items[0], h.sources.Pending) {
		t.Fatalf("err = %v, want only the pending transaction named", err)
	}
	assertPresent(t, h.root, files...)
	if len(h.fake.signals) != 0 || len(h.fake.commands) != 0 {
		t.Fatalf("a pending transaction's candidate was touched: %v %v", h.fake.signals, h.fake.commands)
	}
}

// A previous generation draining held streams is owned until the drain cap,
// with its files; past the cap it is killed and the install fails naming it.
func TestInstallInventoryDrainingGenerationIsBoundedByItsCap(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	files := append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...)
	draining := managedProcess(4002, invPin(digestA), "paperboat-carrier-u1001-"+digestA[:32]+".service", activated.Add(-time.Hour))
	active := managedProcess(4001, invPin(digestB), "paperboat-hostd-u1001-"+digestB[:32]+".service", activated.Add(time.Second))

	within := newInventoryHost(t, digestB, activated, activated.Add(inventoryDrainCap-time.Minute), files, active, draining)
	if err := within.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	within.assertExactlyActive(files, nil, 4001, 4002)

	past := newInventoryHost(t, digestB, activated, activated.Add(inventoryDrainCap+time.Minute), files, active, draining)
	past.fake.termExits[4002] = true
	err := past.sweep.settleInventory(context.Background(), false)
	var leftovers unixLeftoverError
	if !errors.As(err, &leftovers) || len(leftovers.items) != 1 || !strings.Contains(leftovers.items[0], "process 4002") || !strings.Contains(leftovers.items[0], "outlived its drain cap") {
		t.Fatalf("err = %v, want the over-cap generation named", err)
	}
	if _, alive := past.fake.alive[4002]; alive {
		t.Fatalf("over-cap generation still runs: %v", past.fake.signals)
	}
	assertGone(t, past.root, versionFiles(digestA)...)
}

// A draining generation owns its files in the first scan. If it exits while the install removes
// another leftover, its definitions are plain leftovers in the second scan: they go too, and the
// install does not fail for them (fleet: ok=false, the rerun ok=true).
func TestInstallInventoryRemovesTheFilesOfAGenerationThatExitedDuringTheSweep(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	files := append(append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...), versionFiles(digestC)...)
	draining := managedProcess(4002, invPin(digestA), "paperboat-carrier-u1001-"+digestA[:32]+".service", activated.Add(-time.Hour))
	active := managedProcess(4001, invPin(digestB), "paperboat-hostd-u1001-"+digestB[:32]+".service", activated.Add(time.Second))
	h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), files, active, draining)
	h.sweep.run = func(ctx context.Context, name string, arguments ...string) error {
		delete(h.fake.alive, 4002) // the drained generation finishes its last stream
		return h.fake.run(ctx, name, arguments...)
	}
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatalf("install failed for a generation that exited meanwhile: %v", err)
	}
	assertGone(t, h.root, versionFiles(digestA)...)
	assertGone(t, h.root, versionFiles(digestC)...)
	assertPresent(t, h.root, versionFiles(digestB)...)
}

// Second account on the same host: its install and upgrade remove only its own
// old version. The other account's items, whatever version, and the shared
// guard at the current image stay.
func TestInstallInventorySecondUserInstallAndUpgradeLeaveTheOtherAccount(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	other := []string{
		"/usr/local/libexec/paperboat/users/u1002/releases/pins/" + digestA + "/pb",
		"/etc/systemd/system/paperboat-hostd-u1002-" + digestA[:32] + ".service",
		"/usr/local/libexec/paperboat/users/u10010/releases/pins/" + digestA + "/pb",
	}
	otherProcesses := []unixProcess{
		managedProcess(5001, "/usr/local/libexec/paperboat/users/u1002/releases/pins/"+digestA+"/pb", "paperboat-hostd-u1002-"+digestA[:32]+".service", activated.Add(-time.Hour)),
		managedProcess(5002, invGuard, "paperboat-machineguard.service", activated.Add(time.Second)),
	}
	t.Run("install", func(t *testing.T) {
		h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), append(append([]string(nil), versionFiles(digestB)...), other...),
			append([]unixProcess{managedProcess(4001, invPin(digestB), "paperboat-hostd-u1001-"+digestB[:32]+".service", activated.Add(time.Second))}, otherProcesses...)...)
		if err := h.sweep.settleInventory(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		h.assertExactlyActive(append(append([]string(nil), versionFiles(digestB)...), other...), nil, 4001, 5001, 5002)
	})
	t.Run("upgrade", func(t *testing.T) {
		files := append(append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...), other...)
		h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), files,
			append([]unixProcess{
				managedProcess(4001, invPin(digestB), "paperboat-hostd-u1001-"+digestB[:32]+".service", activated.Add(time.Second)),
				unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, started: activated.Add(-time.Hour), unit: "paperboat-machineguard.service"},
			}, otherProcesses...)...)
		h.fake.termExits[4002] = true
		if err := h.sweep.settleInventory(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		h.assertExactlyActive(append(append([]string(nil), versionFiles(digestB)...), other...), versionFiles(digestA), 4001, 5001, 5002)
	})
}

// What cannot be removed fails the install, naming each item; --check reports
// the same without changing anything (the device harness's view).
func TestInstallInventoryNamesWhatItCannotRemoveAndCheckChangesNothing(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	stale := unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, unit: "paperboat-machineguard.service"}
	h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), versionFiles(digestA), stale)
	if err := h.sweep.settleInventory(context.Background(), true); err == nil {
		t.Fatal("check passed with another version on the host")
	}
	if len(h.fake.signals) != 0 || len(h.fake.commands) != 0 {
		t.Fatalf("check changed the host: %v %v", h.fake.signals, h.fake.commands)
	}
	assertPresent(t, h.root, versionFiles(digestA)...)

	h.fake.immortal[4002] = true
	err := h.sweep.settleInventory(context.Background(), false)
	var leftovers unixLeftoverError
	if !errors.As(err, &leftovers) || !reflect.DeepEqual(leftovers.items, []string{"process 4002 (" + invGuard + ") runs a replaced image"}) {
		t.Fatalf("err = %v, want the surviving old guard named", err)
	}
	assertGone(t, h.root, versionFiles(digestA)...)
	if result := (&purgeOutput{}); writePurgeResult(context.Background(), result, err) == nil || !strings.Contains(string(result.data), `"remaining":["process 4002`) {
		t.Fatalf("result does not carry the leftover: %s", result.data)
	}
}

// A user's own pb of an old version, started from a Paperboat terminal (inside the
// session owner's cgroup) or a login shell, is not the install's: it survives.
func TestInstallInventoryNeverTouchesAUsersOwnPB(t *testing.T) {
	activated := time.Unix(1_800_000_000, 0)
	files := append(append([]string(nil), versionFiles(digestB)...), sessionOwnerFiles(digestB)...)
	owner := managedProcess(4001, invPin(digestB), "paperboat-session-owner-u1001-"+digestB[:32]+".service", activated.Add(time.Second))
	shell := unixProcess{pid: 4002, ppid: 4001, exe: "/bin/bash", unit: owner.unit}
	inTerminal := unixProcess{pid: 4003, ppid: 4002, exe: invRoot + "/bin/pb", stale: true, unit: owner.unit, started: activated.Add(-time.Hour)}
	// Reparented to init after its terminal closed, still in the session owner's cgroup.
	orphaned := unixProcess{pid: 4004, ppid: 1, exe: invPin(digestA), unit: owner.unit, started: activated.Add(-time.Hour)}
	loginShell := unixProcess{pid: 4005, ppid: 900, exe: invRoot + "/bin/pb", stale: true, unit: "session-3.scope"}
	h := newInventoryHost(t, digestB, activated, activated.Add(inventoryDrainCap+time.Hour), files, owner, shell, inTerminal, orphaned, loginShell)
	h.sweep.jobs = func(context.Context) (map[int]string, error) { return map[int]string{4001: owner.unit}, nil }
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(files, nil, 4001, 4002, 4003, 4004, 4005)
	if len(h.fake.signals) != 0 {
		t.Fatalf("user processes were signalled: %v", h.fake.signals)
	}
}

// The guard hands over by exec'ing the new binary in place: same pid, same start
// time, new image. macOS compares the image's inode (lsof txt) with the file's.
func TestInstallInventoryOwnsAGuardAfterAnInPlaceExecHandover(t *testing.T) {
	const guard = "/Library/PrivilegedHelperTools/io.paperboat.machineguard"
	activated := time.Unix(1_800_000_000, 0)
	root := t.TempDir()
	plantFile(t, root, guard)
	info, err := os.Stat(filepath.Join(root, guard))
	if err != nil {
		t.Fatal(err)
	}
	installed := uint64(info.Sys().(*syscall.Stat_t).Ino)
	plan, err := newUnixPurgePlan("darwin", 501)
	if err != nil {
		t.Fatal(err)
	}
	// Started a day before the new guard binary was written.
	process := unixProcess{pid: 88, ppid: 1, exe: guard, started: activated.Add(-24 * time.Hour)}
	for _, tc := range []struct {
		name   string
		inodes []uint64
		owned  bool
	}{{"exec'd into the installed file", []uint64{installed + 1, installed}, true}, {"still the replaced image", []uint64{installed + 1}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newPurgeFake(process)
			sweep := testSweep(root, plan, fake)
			sweep.jobs = func(context.Context) (map[int]string, error) {
				return map[int]string{88: "io.paperboat.machineguard"}, nil
			}
			sweep.imageInodes = func(context.Context, int) ([]uint64, error) { return tc.inodes, nil }
			sweep.sources = func(context.Context) (hostinstall.InventorySources, error) {
				return hostinstall.InventorySources{Active: digestA, Activated: activated}, nil
			}
			sweep.clock = func() time.Time { return activated.Add(time.Minute) }
			findings, err := sweep.versionLeftovers(context.Background(), hostinstall.InventorySources{Active: digestA, Activated: activated})
			if err != nil || (len(findings) == 0) != tc.owned {
				t.Fatalf("findings = %v, %v; owned want %v", findings, err, tc.owned)
			}
		})
	}
}

func TestServiceManagerOutputsNameMainProcesses(t *testing.T) {
	systemd := "Id=paperboat-hostd-u1001.service\nMainPID=412\n\nId=paperboat-carrier-u1001-x.service\nMainPID=0\n\nId=paperboat-machineguard.service\nMainPID=77\n"
	if got := parseSystemdMainPIDs(systemd); !reflect.DeepEqual(got, map[int]string{412: "paperboat-hostd-u1001.service", 77: "paperboat-machineguard.service"}) {
		t.Fatalf("systemd = %v", got)
	}
	launchd := "PID\tStatus\tLabel\n-\t0\tcom.apple.x\n91\t0\tio.paperboat.machineguard\n92\t-9\tcom.pinksaucepasta.paperboat.hostd.u501\n"
	if got := parseLaunchctlList(launchd); !reflect.DeepEqual(got, map[int]string{91: "io.paperboat.machineguard", 92: "com.pinksaucepasta.paperboat.hostd.u501"}) {
		t.Fatalf("launchd = %v", got)
	}
	if got := parseLaunchctlPrintPID("gui/501/com.pinksaucepasta.paperboatd = {\n\tstate = running\n\tpid = 1234\n}"); got != 1234 {
		t.Fatalf("launchctl print pid = %d", got)
	}
	if got := parseLsofInodes("p88\nftxt\ni4242\nftxt\ni99\n"); !reflect.DeepEqual(got, []uint64{4242, 99}) {
		t.Fatalf("lsof inodes = %v", got)
	}
	plan, _ := newUnixPurgePlan("linux", 1001)
	sweep := unixSweep{plan: plan}
	for name, want := range map[string]bool{
		"paperboat-hostd-u1001-" + digestA[:32] + ".service": true, "paperboat-machineguard.service": true, "paperboatd.service": true,
		"paperboat-hostd-u1002.service": false, "paperboat-hostd-u10010.service": false, "session-3.scope": false, "user@1001.service": false,
	} {
		if sweep.ourUnit(name) != want {
			t.Fatalf("ourUnit(%q) = %v", name, !want)
		}
	}
}

func TestGenerationTokenReadsEveryGenerationName(t *testing.T) {
	for name, want := range map[string]string{
		"paperboat-hostd-u1001-" + digestA[:32] + ".service":                   digestA[:32],
		"com.pinksaucepasta.paperboat.carrier.u501." + digestA[:32] + ".plist": digestA[:32],
		"generation-" + digestA[:32] + ".json":                                 digestA[:32],
		"hostd.sock.c" + digestA[:16]:                                          digestA[:16],
		"u501-" + digestA[:32]:                                                 digestA[:32],
		"paperboat-hostd-u1001.service":                                        "",
		"paperboat-hostd-u1001-" + strings.ToUpper(digestA[:32]) + ".service":  "",
		"paperboat-hostd-u1001-" + digestA[:31] + ".service":                   "",
	} {
		if got := generationToken(name); got != want {
			t.Fatalf("generationToken(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInventoryPathsAreTheServiceLayout(t *testing.T) {
	plan, err := newUnixPurgePlan("darwin", 501)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := newInventoryPaths(plan)
	if err != nil {
		t.Fatal(err)
	}
	if paths.pins != "/Library/PrivilegedHelperTools/Paperboat/users/u501/releases/pins" || !containsString(paths.generationGlobs, "/Library/Application Support/Paperboat/ipc/u501-*") {
		t.Fatalf("darwin inventory paths = %+v", paths)
	}
	if _, err := os.Stat(filepath.Dir(paths.pins)); err == nil && os.Geteuid() != 0 {
		t.Log("host has a Paperboat install; layout only checked by value")
	}
}
