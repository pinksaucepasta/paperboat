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

// withdrawRunner is systemd for one session-owner unit: loaded and running
// until `disable --now`, absent afterwards.
type withdrawRunner struct {
	commands []string
	removed  bool
}

func (r *withdrawRunner) Run(_ context.Context, name string, args ...string) error {
	command := name + " " + strings.Join(args, " ")
	r.commands = append(r.commands, command)
	if strings.Contains(command, "disable --now") {
		r.removed = true
	}
	return nil
}
func (r *withdrawRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	if r.removed {
		return "LoadState=not-found\n", r.Run(ctx, name, args...)
	}
	return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\n", r.Run(ctx, name, args...)
}

// pbvm1: every `pb update` that failed at "apply service declaration" left the
// candidate's session owner running, with its catalogue descriptor. The failed
// attempt withdraws exactly the session owner it published, never one that was
// already there.
func TestFailedUpdateWithdrawsOnlyItsCandidateSessionOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected native catalog custody")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-withdraw-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err = os.Chmod(root, 0755); err != nil {
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
	pin := filepath.Join(root, "pin")
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "withdraw-candidate", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	for _, existed := range []bool{false, true} {
		runner := &withdrawRunner{}
		catalog := sessionowner.Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: runner}
		d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: source.SHA256[:32], Instance: "u0", Source: source, Executable: pin,
			RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(root, "owner", "control.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
		ctx := context.Background()
		if err = catalog.Publish(ctx, d); err != nil {
			t.Fatal(err)
		}
		descriptor, err := sessionowner.Find(catalog.Root, d.Generation)
		if err != nil {
			t.Fatal(err)
		}
		candidate := candidateSessionOwner{catalog: catalog, generation: d.Generation, existed: existed}
		// No installation record names the candidate: the attempt never committed.
		if err = candidate.withdraw(ctx, filepath.Join(root, "install-metadata.json"), 0); err != nil {
			t.Fatalf("existed=%v: %v", existed, err)
		}
		_, statErr := os.Lstat(descriptor)
		if existed {
			if statErr != nil || len(runner.commands) != 0 {
				t.Fatalf("a session owner published before the attempt was withdrawn: %v %v", statErr, runner.commands)
			}
			continue
		}
		if !os.IsNotExist(statErr) {
			t.Fatalf("candidate descriptor survived the failed update: %v", statErr)
		}
		if !strings.Contains(strings.Join(runner.commands, "\n"), "systemctl disable --now paperboat-session-owner-u0-"+d.Generation) {
			t.Fatalf("candidate unit not stopped and disabled: %v", runner.commands)
		}
		// The next round republishes it as a generation that existed before its attempt.
	}
}
