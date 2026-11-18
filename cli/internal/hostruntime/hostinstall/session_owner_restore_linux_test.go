//go:build linux

package hostinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
)

// systemd for the owner of the row a downgrade rewrote: absent until the attempt starts it, gone after `disable --now`.
type restoreRunner struct {
	running  bool
	commands []string
}

func (r *restoreRunner) Run(_ context.Context, name string, args ...string) error {
	command := name + " " + strings.Join(args, " ")
	r.commands = append(r.commands, command)
	if strings.Contains(command, "disable --now") {
		r.running = false
	}
	return nil
}
func (r *restoreRunner) Output(_ context.Context, _ string, _ ...string) (string, error) {
	if r.running {
		return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\n", nil
	}
	return "LoadState=not-found\n", nil
}

type restoreFixture struct {
	catalog sessionowner.Catalog
	runner  *restoreRunner
	sources map[string]installsource.Source
	owner   func(build, code string, initial bool) sessionowner.Descriptor
	archive sessionowner.Descriptor
	path    string
}

// newRestoreFixture publishes a retired owner's archive row (the older generation on the newer code) next to the
// newer build's own owner, in a root-owned catalogue.
func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires protected native catalog custody")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-restore-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "etc", "systemd", "system"), 0755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]installsource.Source{}
	pins := map[string]string{}
	for name, body := range map[string][]byte{"older": image, "newer": append(append([]byte(nil), image...), 0)} {
		pins[name] = filepath.Join(root, name+"-pin")
		if err = os.WriteFile(pins[name], body, 0755); err != nil {
			t.Fatal(err)
		}
		if sources[name], err = installsource.Inspect(pins[name], name+"-version", installsource.Custom); err != nil {
			t.Fatal(err)
		}
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	runner := &restoreRunner{}
	catalog := sessionowner.Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: runner}
	owner := func(build, code string, initial bool) sessionowner.Descriptor {
		return sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: sources[build].SHA256[:32], Instance: "u0", Source: sources[code], Executable: pins[code], RuntimeRoot: runtimeRoot, InitialStore: initial, Endpoint: filepath.Join(root, build+".sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	}
	archive := owner("older", "newer", true) // the older owner's history, on the newer code (Coordinator.recordRetired)
	for _, d := range []sessionowner.Descriptor{archive, owner("newer", "newer", false)} {
		if err = catalog.Publish(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	path, err := sessionowner.Find(catalog.Root, archive.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return restoreFixture{catalog: catalog, runner: runner, sources: sources, owner: owner, archive: archive, path: path}
}

// startDowngrade is the attempt up to its failure: the older build takes the archive row back and its owner starts.
func (f restoreFixture) startDowngrade(t *testing.T) sessionowner.Descriptor {
	t.Helper()
	ctx := context.Background()
	bound, err := bindInitialSessionStore(ctx, f.catalog, f.owner("older", "older", false), f.sources["newer"].SHA256[:32])
	if err != nil {
		t.Fatal(err)
	}
	if err = f.catalog.Publish(ctx, bound); err != nil {
		t.Fatal(err)
	}
	f.runner.running, f.runner.commands = true, nil
	return bound
}

// A downgrade rewrites a retired owner's archive row to name the older build. When the attempt then fails, the row
// goes back in the same step, with the owner the attempt started stopped first: there is no window in which the
// archive names code the installation does not run. A committed attempt keeps its row.
func TestAFailedDowngradeRestoresTheArchiveRowItRewrote(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()
	for _, committed := range []bool{false, true} {
		candidate := candidateSessionOwner{catalog: f.catalog, generation: f.archive.Generation, existed: true, previous: &f.archive}
		bound := f.startDowngrade(t)
		installed := ""
		if committed {
			installed = f.archive.Generation
		}
		// The attempt fails here: nothing names the older build in the installation record.
		if err := candidate.withdrawUnless(ctx, installed); err != nil {
			t.Fatalf("committed=%v: %v", committed, err)
		}
		row, err := sessionowner.Read(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if committed {
			if row != bound || len(f.runner.commands) != 0 {
				t.Fatalf("a committed attempt's row was touched: %+v %v", row, f.runner.commands)
			}
			continue
		}
		if row != f.archive {
			t.Fatalf("the failed downgrade left the archive row changed:\n got %+v\nwant %+v", row, f.archive)
		}
		if !strings.Contains(strings.Join(f.runner.commands, "\n"), "systemctl disable --now paperboat-session-owner-u0-"+f.archive.Generation) {
			t.Fatalf("the owner the attempt started was not stopped: %v", f.runner.commands)
		}
		// The row is the archive again, so the next attempt can take it back.
		if err = f.catalog.Publish(ctx, bound); err != nil {
			t.Fatalf("the restored archive cannot be taken back again: %v", err)
		}
		if err = f.catalog.Publish(ctx, f.archive); err != nil {
			t.Fatal(err)
		}
	}
}

// A downgrade killed between rewriting the row and failing leaves the row naming the older build; the install journal
// carries the row it found, and the recovery of the killed attempt puts it back instead of forgetting it.
func TestRecoveryOfAKilledDowngradeRestoresTheArchiveRow(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()
	// The journal is the real protected file: the strict decoder must accept the new field and give the row back whole.
	journalPath := filepath.Join(filepath.Dir(filepath.Dir(f.path)), "journal", "install-journal.json")
	defer os.Remove(journalPath)
	if err := writeJournal(journalPath, installJournal{Schema: journalSchemaV1, Stage: "prepared", Source: f.sources["older"], MetadataCaptured: true, UpdatedAt: time.Now().UTC(), PreviousSessionOwner: &f.archive}); err != nil {
		t.Fatal(err)
	}
	journal, err := loadJournal(journalPath)
	if err != nil || journal.PreviousSessionOwner == nil || *journal.PreviousSessionOwner != f.archive {
		t.Fatalf("the journal does not carry the row it found: %v %+v", err, journal.PreviousSessionOwner)
	}
	f.startDowngrade(t)
	// The attempt's process died: no installation record names the older build.
	if err = withdrawCandidateOwner(ctx, filepath.Join(filepath.Dir(f.path), "no-install-metadata.json"), 0, f.catalog, f.sources["older"], journal.PreviousSessionOwner); err != nil {
		t.Fatal(err)
	}
	row, err := sessionowner.Read(f.path)
	if err != nil || row != f.archive {
		t.Fatalf("the recovery of the killed downgrade left the row changed or forgot it: %v\n got %+v\nwant %+v", err, row, f.archive)
	}
}
