//go:build linux

package hostinstall

import (
	"context"
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
	restore := func(context.Context) (func(context.Context) error, error) { return nil, nil }
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
