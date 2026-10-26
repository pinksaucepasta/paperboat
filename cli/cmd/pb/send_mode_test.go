package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// Commit 04ecd8c5 hard-coded "automatic" into the descriptor request, so `pb send --unlimited` never reached the daemon
// as Unlimited and sent over a hosted relay. The request carries the mode the user chose, and a refusal reads as advice.
func TestSendUnlimitedDeclaresItsModeAndRefusalReadsAsAdvice(t *testing.T) {
	var declared string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/machines":
			writeAPIData(t, writer, api.UserMachinePage{Items: []api.UserMachine{{ID: "machine_host", EnvironmentID: "environment_host", Alias: "hn", State: "ready", Online: true, Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/root", InstallationGeneration: 1, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: true}}}}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case "POST /v1/team-inbox/requests":
			writeAPIData(t, writer, api.TeamInboxRequest{Status: "not_required"})
		case "POST /v1/machines/machine_host/file-transfer-descriptor":
			body, _ := io.ReadAll(request.Body)
			declared = string(body)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"error":{"code":"destination_update_required","message":"x","details":{"machine":"hn","max_bytes":524288000}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer backend.Close()

	_, configPath := localDaemonReadinessTestConfig(t, true)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "")
	file := filepath.Join(filepath.Dir(configPath), "payload.bin")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestProfile(t, filepath.Dir(configPath), configPath, backend.URL)
	state, err := identity.Open(identity.Config{StateRoot: os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := state.Registration()
	if err != nil {
		t.Fatal(err)
	}
	registration.ServerURL = backend.URL
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
	command := newRootCommand()
	var out, stderr bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--config", configPath, "--no-customization", "send", file, "--to", "hn", "--unlimited"})
	commandCtx, finish := context.WithTimeout(context.Background(), 10*time.Second)
	defer finish()
	err = command.ExecuteContext(commandCtx)
	if err == nil || !strings.Contains(declared, `"transfer_mode":"unlimited"`) {
		t.Fatalf("err=%v descriptor request=%q", err, declared)
	}
	if got := err.Error(); !strings.Contains(got, "hn cannot receive files over 500 MB or unlimited yet") {
		t.Fatalf("refusal text=%q", got)
	}
}
