package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// `pb send <file> --to <this computer>` copies the file into this computer's own Inbox, where it is
// listed like a received file, and makes no file transfer. A second send never replaces the first.
func TestSendToThisComputerCopiesIntoItsOwnInbox(t *testing.T) {
	_, configPath := localDaemonReadinessTestConfig(t, true)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "")
	directory := filepath.Dir(configPath)
	file := filepath.Join(directory, "notes.txt")
	if err := os.WriteFile(file, []byte("notes for later"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := identity.Open(identity.Config{StateRoot: os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := state.Registration()
	if err != nil {
		t.Fatal(err)
	}
	inboxPath := filepath.Join(directory, "Inbox")
	if err := os.Mkdir(inboxPath, 0o700); err != nil {
		t.Fatal(err)
	}
	var requests []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/v1/machines":
			machine := api.UserMachine{ID: registration.MachineID, Alias: "thiscomputer", State: "ready", Online: true, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: true}}}
			writeAPIData(t, w, api.UserMachinePage{Items: []api.UserMachine{machine}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case "/v1/team-inbox/requests":
			writeAPIData(t, w, api.TeamInboxRequest{RequestID: "request_self", Status: "not_required"})
		default:
			t.Errorf("a send to this computer reached %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
	writeTestProfile(t, directory, configPath, backend.URL)
	registration.ServerURL, registration.InboxPath = backend.URL, inboxPath
	registration.UpdatedAt = time.Now().UTC()
	if err = state.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
	server, _ := localDaemonReadinessServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer stopLocalDaemonReadinessServer(t, cancel, done)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	waitForCommandSocket(t, paths.SocketPath)

	send := func() cliJSONEnvelope {
		t.Helper()
		command := newRootCommand()
		var out, stderr bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&stderr)
		command.SetArgs([]string{"--config", configPath, "--no-customization", "send", file, "--to", "thiscomputer", "--json"})
		commandCtx, finish := context.WithTimeout(context.Background(), 10*time.Second)
		defer finish()
		if err := command.ExecuteContext(commandCtx); err != nil {
			t.Fatalf("send to this computer failed: %v (%s)", err, stderr.String())
		}
		var receipt cliJSONEnvelope
		if err := json.Unmarshal(out.Bytes(), &receipt); err != nil || !receipt.OK {
			t.Fatalf("receipt %s: %v", out.String(), err)
		}
		return receipt
	}
	send()
	send()
	for _, name := range []string{"notes.txt", "notes (2).txt"} {
		content, err := os.ReadFile(filepath.Join(inboxPath, name))
		if err != nil || string(content) != "notes for later" {
			t.Fatalf("%s in the Inbox: %q %v", name, content, err)
		}
	}
	for _, path := range requests {
		if path != "/v1/machines" && path != "/v1/team-inbox/requests" {
			t.Fatalf("requests=%v", requests)
		}
	}
}
