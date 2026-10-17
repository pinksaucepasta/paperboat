package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestSendUsesRenewedCredentialWhileWaitingForApproval(t *testing.T) {
	_, configPath := localDaemonReadinessTestConfig(t, true)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "")
	file := filepath.Join(filepath.Dir(configPath), "approval.bin")
	if err := os.WriteFile(file, []byte("approval fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var renewed atomic.Bool
	var backend *httptest.Server
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines":
			machine := api.UserMachine{ID: "machine_destination", Alias: "destination", State: "ready", Online: true, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: true}}}
			writeAPIData(t, w, api.UserMachinePage{Items: []api.UserMachine{machine}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case "/v1/team-inbox/requests":
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			store, err := config.ProfileStoreFor(cfg)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			expires := time.Now().Add(time.Hour)
			err = store.Replace(config.Profile{Issuer: backend.URL, CLIClientSessionID: "cls_test", AccessExpiresAt: expires}, config.Credential{AccessToken: "renewed_fixture", RefreshToken: "renewed_refresh", ExpiresAt: expires})
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			writeAPIData(t, w, api.TeamInboxRequest{RequestID: "request_fixture", Status: "pending"})
		case "/v1/team-inbox/requests/request_fixture":
			if r.Header.Get("Authorization") != "Bearer renewed_fixture" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"expired fixture credential"}}`))
				return
			}
			renewed.Store(true)
			writeAPIData(t, w, api.TeamInboxRequest{RequestID: "request_fixture", Status: "declined"})
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
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
	command.SetArgs([]string{"--config", configPath, "--no-customization", "send", file, "--to", "destination", "--json"})
	commandCtx, finish := context.WithTimeout(context.Background(), 10*time.Second)
	defer finish()
	err = command.ExecuteContext(commandCtx)
	if !renewed.Load() || err == nil || errors.Is(err, api.ErrUnauthenticated) {
		t.Fatalf("renewed account credential used=%t; result=%v", renewed.Load(), err)
	}
	var rendered bytes.Buffer
	if writeErr := writeCLIJSONError(&rendered, err); writeErr != nil {
		t.Fatal(writeErr)
	}
	var receipt cliJSONEnvelope
	if decodeErr := json.Unmarshal(rendered.Bytes(), &receipt); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if receipt.OK || receipt.Error == nil || receipt.Error.Code != "operation_refused" ||
		receipt.Error.Message != "The recipient declined this transfer. Contact them before sending again." ||
		receipt.Error.Retryable || receipt.Error.StateChanged != true {
		t.Fatalf("declined transfer lost its public refusal: %s", rendered.String())
	}
}
