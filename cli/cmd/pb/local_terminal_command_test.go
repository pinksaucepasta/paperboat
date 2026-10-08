package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewTerminalCommandRejectsSessionAndPositionalTarget(t *testing.T) {
	for _, args := range [][]string{{"new", "--session", "existing"}, {"new", "other"}} {
		var out, errs bytes.Buffer
		code := run(context.Background(), args, &out, &errs)
		if code != 2 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errs.String())
		}
	}
}

func TestNewTerminalCommandHelp(t *testing.T) {
	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"new", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "current directory") {
		t.Fatalf("missing local directory help: %s", output.String())
	}
}

func TestLocalTerminalTargetUsesEnrollmentAndCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "runtime"))
	store, err := runtimeIdentityStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_local", EnvironmentID: "machine_local", PublicKeyID: store.Current().ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(store.Current().Public()), InboxPath: root, InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	machine, cwd, err := localTerminalTarget()
	want, _ := os.Getwd()
	if err != nil || machine != "machine_local" || cwd != want {
		t.Fatalf("target=%q cwd=%q err=%v", machine, cwd, err)
	}
}

func TestNewTerminalWithoutEnrollmentExplainsSetup(t *testing.T) {
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", t.TempDir())
	var out, errs bytes.Buffer
	code := run(context.Background(), []string{"new"}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "pb setup") {
		t.Fatalf("code=%d stderr=%s", code, errs.String())
	}
}
