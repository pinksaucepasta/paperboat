//go:build linux

package hostinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A downgrade rewrites a retired owner's archive row to name the older build. When the attempt then fails, the row
// goes back in the same step, with the owner the attempt started stopped first: there is no window in which the
// archive names code the installation does not run. A committed attempt keeps its row.
func TestAFailedDowngradeRestoresTheArchiveRowItRewrote(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected native catalog custody")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-restore-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
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
	owner := func(build string, code string, initial bool) sessionowner.Descriptor {
		return sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: sources[build].SHA256[:32], Instance: "u0", Source: sources[code], Executable: pins[code], RuntimeRoot: runtimeRoot, InitialStore: initial, Endpoint: filepath.Join(root, build+".sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	}
	ctx := context.Background()
	archive := owner("older", "newer", true) // the older owner's history, on the newer code (Coordinator.recordRetired)
	for _, d := range []sessionowner.Descriptor{archive, owner("newer", "newer", false)} {
		if err = catalog.Publish(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	path, err := sessionowner.Find(catalog.Root, archive.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for _, committed := range []bool{false, true} {
		// The attempt: take back the archive row for the older build and start its owner.
		candidate := candidateSessionOwner{catalog: catalog, generation: archive.Generation, existed: true, previous: &archive}
		bound, err := bindInitialSessionStore(ctx, catalog, owner("older", "older", false), sources["newer"].SHA256[:32])
		if err != nil {
			t.Fatal(err)
		}
		if err = catalog.Publish(ctx, bound); err != nil {
			t.Fatal(err)
		}
		runner.running, runner.commands = true, nil
		installed := ""
		if committed {
			installed = archive.Generation
		}
		// The attempt fails here: nothing names the older build in the installation record.
		if err = candidate.withdrawUnless(ctx, installed); err != nil {
			t.Fatalf("committed=%v: %v", committed, err)
		}
		row, err := sessionowner.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		if committed {
			if row != bound || len(runner.commands) != 0 {
				t.Fatalf("a committed attempt's row was touched: %+v %v", row, runner.commands)
			}
			continue
		}
		if row != archive {
			t.Fatalf("the failed downgrade left the archive row changed:\n got %+v\nwant %+v", row, archive)
		}
		if !strings.Contains(strings.Join(runner.commands, "\n"), "systemctl disable --now paperboat-session-owner-u0-"+archive.Generation) {
			t.Fatalf("the owner the attempt started was not stopped: %v", runner.commands)
		}
		// The row is the archive again, so the next attempt can take it back.
		if err = catalog.Publish(ctx, bound); err != nil {
			t.Fatalf("the restored archive cannot be taken back again: %v", err)
		}
		if err = catalog.Publish(ctx, archive); err != nil {
			t.Fatal(err)
		}
	}
}
