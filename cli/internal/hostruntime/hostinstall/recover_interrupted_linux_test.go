//go:build linux

package hostinstall

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// pbvm1 F14-03: an update killed after it started its candidate's session owner left that descriptor and the
// candidate's pin behind; once recovery reactivated the previous worker, the inventory took the descriptor for
// history and kept both through a later good update. Recovery withdraws them like a failed attempt does.
func TestInterruptedUpdateLeavesNoCandidateSessionOwnerOrPin(t *testing.T) {
	paths, request, _ := interruptedInstall(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(paths.root, "candidate")
	if err = os.WriteFile(staged, image, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate, err := installsource.Inspect(staged, "candidate", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	pinDir := filepath.Join(paths.root, "pins", candidate.SHA256)
	if err = os.MkdirAll(pinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(staged, filepath.Join(pinDir, "pb")); err != nil {
		t.Fatal(err)
	}
	journal, err := loadJournal(paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	journal.Source = candidate // the killed install was installing the candidate
	if err = writeJournal(paths.journal, journal); err != nil {
		t.Fatal(err)
	}
	runner := &withdrawRunner{}
	catalog := sessionOwnerCatalogAt(paths, request)
	catalog.Runner = runner
	runtimeRoot := filepath.Join(paths.root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: candidate.SHA256[:32], Instance: paths.instance, Source: candidate,
		Executable: filepath.Join(pinDir, "pb"), RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(paths.root, "owner", "control.sock"),
		UID: request.UID, GID: request.GID, Workspace: paths.root, Shell: "/bin/sh"}
	if err = catalog.Publish(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	descriptor, err := sessionowner.Find(catalog.Root, d.Generation)
	if err != nil {
		t.Fatal(err)
	}
	restore := func(context.Context, Request) (func(context.Context) error, error) { return nil, nil }
	if _, err = recoverInterruptedInstall(context.Background(), paths, request.UID, runner, restore); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(descriptor); !os.IsNotExist(err) {
		t.Fatalf("the candidate's session-owner descriptor survived recovery: %v", err)
	}
	if !strings.Contains(strings.Join(runner.commands, "\n"), "disable --now paperboat-session-owner-"+paths.instance+"-"+d.Generation) {
		t.Fatalf("the candidate's session owner was not stopped: %v", runner.commands)
	}
	if _, err = os.Lstat(pinDir); !os.IsNotExist(err) {
		t.Fatalf("the candidate's pin survived recovery: %v", err)
	}
	if _, err = os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("journal left behind: %v", err)
	}
}

// Withdrawing the killed attempt's candidate is cleanup. An unreadable candidate descriptor used to fail the whole
// recovery before the services were restored, so the updater stayed in its retry backoff forever with the machine
// half updated. The services and files are restored, the journal removed, and the leftover reported.
func TestAnUnreadableCandidateDescriptorDoesNotStopRecovery(t *testing.T) {
	paths, request, _ := interruptedInstall(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(paths.root, "candidate")
	if err = os.WriteFile(staged, append(image, 0), 0o755); err != nil {
		t.Fatal(err)
	}
	candidate, err := installsource.Inspect(staged, "candidate", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := loadJournal(paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	journal.Source = candidate
	if err = writeJournal(paths.journal, journal); err != nil {
		t.Fatal(err)
	}
	catalog := sessionOwnerCatalogAt(paths, request)
	if err = os.Mkdir(catalog.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	descriptor, err := sessionowner.Find(catalog.Root, candidate.SHA256[:32])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(descriptor, []byte("{not a descriptor"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func(context.Context, Request) (func(context.Context) error, error) { restored = true; return nil, nil }
	recovery, err := recoverInterruptedInstall(context.Background(), paths, request.UID, &withdrawRunner{}, restore)
	if err != nil || !restored || recovery.Cleanup == nil {
		t.Fatalf("err=%v restored=%v cleanup=%v", err, restored, recovery.Cleanup)
	}
	if _, err = os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("journal left behind: %v", err)
	}
}

// A killed first enrollment has no installation record yet, so recovery could not rebuild its services or catalogue
// and left its session owner, descriptor and pin for "the next install". The journal now carries the request it was
// installing: recovery restores its services from it and removes the candidate it staged.
func TestKilledFirstEnrollmentLeavesNoCandidateSessionOwnerOrPin(t *testing.T) {
	paths, request, _ := interruptedInstall(t)
	if err := os.Remove(paths.metadata); err != nil {
		t.Fatal(err)
	}
	journal, err := loadJournal(paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	journal.PreviousMetadata = nil
	if journal.Pending, err = json.Marshal(request); err != nil {
		t.Fatal(err)
	}
	if err = writeJournal(paths.journal, journal); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(paths.worker)
	if err != nil {
		t.Fatal(err)
	}
	pinDir := filepath.Join(paths.root, "pins", request.Source.SHA256)
	if err = os.MkdirAll(pinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pinDir, "pb"), image, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &withdrawRunner{}
	catalog := sessionOwnerCatalogAt(paths, request)
	catalog.Runner = runner
	runtimeRoot := filepath.Join(paths.root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: request.Source.SHA256[:32], Instance: paths.instance, Source: request.Source,
		Executable: filepath.Join(pinDir, "pb"), RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(paths.root, "owner", "control.sock"),
		UID: request.UID, GID: request.GID, Workspace: paths.root, Shell: "/bin/sh"}
	if err = catalog.Publish(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	descriptor, err := sessionowner.Find(catalog.Root, d.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var restoredFrom Request
	restore := func(_ context.Context, request Request) (func(context.Context) error, error) {
		restoredFrom = request
		return nil, nil
	}
	recovery, err := recoverInterruptedInstall(context.Background(), paths, request.UID, runner, restore)
	if err != nil || recovery.Cleanup != nil {
		t.Fatalf("err=%v cleanup=%v", err, recovery.Cleanup)
	}
	if restoredFrom.Source != request.Source {
		t.Fatal("the services were not restored from the enrollment's own request")
	}
	for _, leftover := range []string{descriptor, pinDir, paths.journal, paths.metadata} {
		if _, err = os.Lstat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s survived the recovery of a killed first enrollment: %v", leftover, err)
		}
	}
}
