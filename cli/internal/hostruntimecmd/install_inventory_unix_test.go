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
	h.sources = hostinstall.InventorySources{Active: active, Activated: activated, Retained: map[string]bool{}}
	h.sweep = testSweep(root, plan, h.fake)
	h.sweep.sources = func(context.Context) (hostinstall.InventorySources, error) { return h.sources, nil }
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
	return unixProcess{pid: pid, ppid: 1, exe: exe, started: started, managed: true, unit: unit}
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
		unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, managed: true, started: activated.Add(-time.Hour)},
		unixProcess{pid: 4003, ppid: 1, exe: invRoot + "/bin/pb", stale: true, managed: true, started: activated.Add(-time.Hour)},
		managedProcess(4004, invGuard, "paperboat-machineguard.service", activated.Add(time.Second)),
	)
	h.fake.termExits[4002], h.fake.termExits[4003] = true, true
	if err := h.sweep.settleInventory(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(versionFiles(digestB), old, 4001, 4004)
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
	files := append(append([]string(nil), versionFiles(digestA)...), versionFiles(digestB)...)
	h := newInventoryHost(t, digestB, activated, activated.Add(time.Minute), files,
		managedProcess(4001, invPin(digestA), "paperboat-hostd-u1001-"+digestA[:32]+".service", activated.Add(time.Second)),
		unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, managed: true},
	)
	h.fake.termExits[4002] = true
	journal := "/var/lib/paperboat-installer/users/u1001/install-journal.json"
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
	h.sources.Retained[digestC] = true
	for _, pid := range []int{4002, 4003, 4005} {
		h.fake.termExits[pid] = true
	}
	err := h.sweep.settleInventory(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	h.assertExactlyActive(keep, newer, 4001, 4004)
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
				unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, managed: true, started: activated.Add(-time.Hour)},
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
	stale := unixProcess{pid: 4002, ppid: 1, exe: invGuard, stale: true, managed: true}
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
