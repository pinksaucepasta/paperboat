//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/spf13/cobra"
)

func TestSetupCompletesAfterInstallerRemovesInvokingExecutable(t *testing.T) {
	if os.Getenv("PAPERBOAT_TEST_REMOVED_SETUP_EXECUTABLE") == "1" {
		testSetupWithRemovedExecutable(t)
		return
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	candidate := filepath.Join(t.TempDir(), "pb.test")
	output, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, candidate, "-test.run=^TestSetupCompletesAfterInstallerRemovesInvokingExecutable$", "-test.count=1")
	child.Env = append(os.Environ(), "PAPERBOAT_TEST_REMOVED_SETUP_EXECUTABLE=1")
	if result, err := child.CombinedOutput(); err != nil {
		t.Fatalf("post-install setup failed: %v\n%s", err, result)
	}
}

func testSetupWithRemovedExecutable(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "identity")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
	t.Setenv("HOME", root)
	t.Setenv("XDG_RUNTIME_DIR", root)
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	machine := api.UserMachine{ID: "machine-kept", EnvironmentID: "machine-kept", Alias: "studio", InstallationGeneration: 5, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public())}
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/machines" {
			t.Error("unexpected post-install mutation")
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		writeAPIData(t, w, map[string]any{"items": []api.UserMachine{machine}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
	}))
	defer backend.Close()
	before := identity.Registration{ServerURL: backend.URL, AccountID: "account-kept", MachineID: machine.ID, EnvironmentID: machine.EnvironmentID, PublicKeyID: key.ID, PublicIdentityKey: machine.PublicIdentityKey, InboxPath: filepath.Join(root, "chosen-inbox"), InstallationGeneration: 5, SSHUser: "owner", SSHPort: 22, UpdatedAt: time.Now().UTC()}
	if err := store.SaveRegistration(before); err != nil {
		t.Fatal(err)
	}
	before, err = store.Registration()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	backup := executable + ".install-backup"
	if err := os.Rename(executable, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.Flags().String("recovery-output", "", "")
	var output bytes.Buffer
	command.SetOut(&output)
	client := api.New(backend.URL, config.Credential{AccessToken: "fixture-token"}, backend.Client())
	if err := finishMachineSetup(command, client, store, machine.ID, before.InboxPath, true); err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			Machine   api.UserMachine `json:"machine"`
			InboxPath string          `json:"inbox_path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	after, err := store.Registration()
	if err != nil {
		t.Fatal(err)
	}
	before.UpdatedAt = after.UpdatedAt
	if !result.OK || !reflect.DeepEqual(before, after) || result.Data.Machine.ID != machine.ID || result.Data.InboxPath != before.InboxPath || calls.Load() != 1 {
		t.Fatal("completed setup did not preserve identity, Inbox and output")
	}
}
