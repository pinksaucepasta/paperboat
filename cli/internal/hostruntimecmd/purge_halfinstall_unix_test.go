//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	documentation "github.com/pinksaucepasta/paperboat/docs"
)

// F1-09 (pbmac, dev-6b9ea3ba3): a failed re-pair left a half-installed Mac that
// neither reset nor uninstall could clear. The seeded state is the per-generation
// layout: generation launchd jobs, their descriptors and pins, the F2-06 host
// service job and its availability state, and a pending (never enrolled) record.
func plantDarwinHalfInstall(t *testing.T, root string) []string {
	t.Helper()
	const generation = "0123456789abcdef0123456789abcdef"
	const other = "fedcba9876543210fedcba9876543210"
	sha := generation + generation
	planted := []string{}
	for _, label := range []string{
		"com.pinksaucepasta.paperboat.runtime-privileged.u501",
		"com.pinksaucepasta.paperboat.updated.u501",
		"com.pinksaucepasta.paperboat.update-activate.u501",
		"com.pinksaucepasta.paperboat.hostd.u501." + generation,
		"com.pinksaucepasta.paperboat.hostd.u501." + other,
		"com.pinksaucepasta.paperboat.carrier.u501." + generation,
		"com.pinksaucepasta.paperboat.session-owner.u501." + generation,
	} {
		planted = append(planted, "/Library/LaunchDaemons/"+label+".plist")
	}
	helper := "/Library/PrivilegedHelperTools/Paperboat/users/u501/"
	support := "/Library/Application Support/Paperboat/"
	planted = append(planted,
		helper+"bin/pb",
		helper+"releases/pb.staged",
		helper+"releases/pins/"+sha+"/pb",
		helper+"carrier/config.json",
		helper+"carrier/generation-"+generation+".json",
		helper+"carrier/owner-"+generation+".lock",
		helper+"session-owners/"+generation+"/descriptor.json",
		support+"users/u501/install-metadata.json",
		support+"users/u501/install-journal.json",
		support+"users/u501/hostd.token",
		support+"updated-u501/state.json",
		support+"run/paperboat-hostd-u501/hostd.sock",
		support+"run/paperboat-updated-u501/control.sock",
		support+"run/paperboat-host/host-service.sock",
		support+"ipc/u501-"+generation+"/control.sock",
		support+"users/u501/power-baseline.json",
		support+"users/u501/availability-policy.json",
		"/private/var/log/com.pinksaucepasta.paperboat.hostd.u501."+generation+".log",
	)
	for _, path := range planted {
		plantFile(t, root, path)
	}
	return planted
}

func TestDarwinPurgeClearsAHalfInstallAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	planted := plantDarwinHalfInstall(t, root)
	plan, err := newUnixPurgePlan("darwin", 501)
	if err != nil {
		t.Fatal(err)
	}
	pin := "/Library/PrivilegedHelperTools/Paperboat/users/u501/releases/pins/" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/pb"
	fake := newPurgeFake(unixProcess{pid: 900, ppid: 1, exe: pin}, unixProcess{pid: 901, ppid: 1, exe: "/usr/sbin/foreign"})
	fake.termExits[900] = true
	manuals := filepath.Join(root, "usr/local/share/man")
	if err := documentation.Install(manuals); err != nil {
		t.Fatal(err)
	}
	pages, err := filepath.Glob(filepath.Join(manuals, "man1", "pb*.1"))
	if err != nil || len(pages) == 0 {
		t.Fatal("manual fixture missing", err)
	}
	if err := testSweep(root, plan, fake).purgeAccount(context.Background()); err != nil {
		t.Fatalf("half-install purge failed:\n%s", errorChainText(err))
	}
	if left, _ := filepath.Glob(filepath.Join(manuals, "man1", "pb*.1")); len(left) != 0 {
		t.Fatalf("%d of %d manual pages survived the last account", len(left), len(pages))
	}
	for _, path := range planted {
		assertGone(t, root, path)
	}
	assertGone(t, root,
		"/Library/PrivilegedHelperTools/Paperboat",
		"/Library/Application Support/Paperboat/users",
		"/Library/Application Support/Paperboat/run",
		"/Library/Application Support/Paperboat/ipc",
	)
	if _, alive := fake.alive[900]; alive {
		t.Fatal("generation process from the account pin survived")
	}
	if _, alive := fake.alive[901]; !alive {
		t.Fatal("foreign process was signalled")
	}
	if fake.uninstalls != 1 || fake.guards != 1 {
		t.Fatalf("uninstalls=%d guards=%d, want 1 and 1", fake.uninstalls, fake.guards)
	}

	// A second run over the clean machine is a no-op that still succeeds.
	again := newPurgeFake()
	if err := testSweep(root, plan, again).purgeAccount(context.Background()); err != nil {
		t.Fatalf("second purge failed:\n%s", errorChainText(err))
	}
	if again.uninstalls != 0 || len(again.signals) != 0 {
		t.Fatalf("second purge acted: uninstalls=%d signals=%v", again.uninstalls, again.signals)
	}
	entries, err := os.ReadDir(filepath.Join(root, "Library", "LaunchDaemons"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("launch daemons left: %v %v", entries, err)
	}
}

// F1-09: reset after a failed pair returned before its sweep when the account
// CLI could not be proven, so it removed nothing and the Mac stayed stuck.
func TestResetPurgeNeverStopsAtAnUnprovableCLI(t *testing.T) {
	keep := "/Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb"
	for _, proven := range []bool{true, false} {
		root := t.TempDir()
		planted := plantDarwinHalfInstall(t, root)
		plan, err := newUnixPurgePlan("darwin", 501)
		if err != nil {
			t.Fatal(err)
		}
		fake := newPurgeFake()
		var uninstalledKeeping []string
		prove := func() (string, error) { return keep, nil }
		if !proven {
			prove = func() (string, error) { return "", errors.New("not a Paperboat executable") }
		}
		err = testSweep(root, plan, fake).purgeForReset(context.Background(), prove, func(_ context.Context, kept string) error {
			uninstalledKeeping = append(uninstalledKeeping, kept)
			return nil
		})
		for _, path := range planted {
			if proven && path == keep {
				assertPresent(t, root, keep)
				continue
			}
			assertGone(t, root, path)
		}
		if proven {
			if err != nil || len(uninstalledKeeping) != 1 || uninstalledKeeping[0] != keep {
				t.Fatalf("proven reset: err=%v uninstall=%v", err, uninstalledKeeping)
			}
			continue
		}
		var staged purgeStageFailure
		if !errors.As(err, &staged) || staged.stage != "reset_cli" || fake.uninstalls != 1 || fake.guards != 1 {
			t.Fatalf("unprovable CLI: err=%v uninstalls=%d guards=%d", err, fake.uninstalls, fake.guards)
		}
	}
}
