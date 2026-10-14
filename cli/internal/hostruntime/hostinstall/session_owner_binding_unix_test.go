//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"os"
	"path/filepath"
	"testing"
)

// Installation policy changes must not republish executable authority for an
// existing generation, or replace its private capability and historical store.
func TestSessionOwnerPolicyChangeReusesProtectedBinding(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected native catalog custody")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "pb-owner-binding-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(self, "qualification", installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "official-pin")
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	c := sessionowner.Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 2}
	original := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: source.SHA256[:32], Instance: "u0", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(root, "owner.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	ctx := context.Background()
	if err = c.Publish(ctx, original); err != nil {
		t.Fatal(err)
	}
	capability, err := sessionowner.ReadCapability(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, custom := range []bool{false, true} {
		desired := original
		desired.InitialStore = false
		desired.Source.AutomaticUpdates = false
		desired.Executable = filepath.Join(root, "candidate-policy-pin")
		if custom {
			desired.Source.Distribution = installsource.Custom
		}
		bound, err := bindInitialSessionStore(ctx, c, desired, original.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if bound != original {
			t.Fatal("policy transition changed protected native binding")
		}
		if err = c.Publish(ctx, bound); err != nil {
			t.Fatal("same-byte installation attempted authority replacement", err)
		}
		after, err := sessionowner.ReadCapability(bound)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(capability, after) {
			t.Fatal("private owner identity changed")
		}
		path, _ := sessionowner.Find(c.Root, original.Generation)
		persisted, err := sessionowner.Read(path)
		if err != nil || persisted != original {
			t.Fatal("protected descriptor changed", err)
		}
	}
	mismatch := original
	mismatch.Source.Version = "different-version"
	if _, err = bindInitialSessionStore(ctx, c, mismatch, original.Generation); err == nil {
		t.Fatal("mismatched artifact version accepted")
	}
}
