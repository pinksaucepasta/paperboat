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
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
)

// prepareTestRegistration points this computer's registration at the fake
// backend, mirroring the other send tests.
func prepareTestRegistration(t *testing.T, backendURL string) {
	t.Helper()
	state, err := identity.Open(identity.Config{StateRoot: os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := state.Registration()
	if err != nil {
		t.Fatal(err)
	}
	registration.ServerURL = backendURL
	if err = state.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
}

// An offline destination answers the single prepare call with machine_offline:
// the send waits out the handover grace and refuses, without a machine list,
// a preference read or a descriptor fetch.
func TestSendPrepareOfflineWaitsOutTheGraceAndRefuses(t *testing.T) {
	var prepares atomic.Int32
	var reached []string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached = append(reached, request.Method+" "+request.URL.Path)
		if request.Method == http.MethodGet && request.URL.Path == "/v1/machines" {
			writeAPIData(t, writer, api.UserMachinePage{Items: []api.UserMachine{{ID: "machine_destination", EnvironmentID: "environment_destination", Alias: "offline-host", State: "ready", Online: true, Platform: "linux", InstallationGeneration: 1, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: true}}}}, Pagination: api.Pagination{Limit: 200, Total: 1}})
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/sends/prepare" {
			prepares.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"error":{"code":"machine_offline","message":"offline"}}`))
			return
		}
		http.NotFound(writer, request)
	}))
	defer backend.Close()

	_, configPath := localDaemonReadinessTestConfig(t, true)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "")
	file := filepath.Join(filepath.Dir(configPath), "offline.bin")
	if err := os.WriteFile(file, []byte("offline"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestProfile(t, filepath.Dir(configPath), configPath, backend.URL)
	prepareTestRegistration(t, backend.URL)
	server, _ := localDaemonReadinessServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer stopLocalDaemonReadinessServer(t, cancel, done)
	paths, err := hostDependenciesOf(t.Context()).daemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	waitForCommandSocket(t, paths.SocketPath)
	command := newRootCommand()
	var out, stderr bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--config", configPath, "--no-customization", "send", file, "--to", "offline-host"})
	commandCtx, finish := context.WithTimeout(withMachineBlip(context.Background(), 400*time.Millisecond, 50*time.Millisecond), 10*time.Second)
	defer finish()
	err = command.ExecuteContext(commandCtx)
	if !machineOfflineError(err) {
		t.Fatalf("err=%v, want the typed machine_offline refusal", err)
	}
	if prepares.Load() < 2 {
		t.Fatalf("prepare calls=%d, want the offline retry", prepares.Load())
	}
	for _, path := range reached {
		if path != "POST /v1/sends/prepare" && path != "GET /v1/machines" {
			t.Fatalf("offline send reached %s", path)
		}
	}
}

// A manually approved send answers 202 with a null descriptor, polls the
// existing request route until approved, then fetches the descriptor on the
// existing route with the approved request binding. The pending poll is the
// only extra round trip.
func TestSendPreparePendingPollsThenFetchesTheDescriptor(t *testing.T) {
	var reached []string
	var descriptorBody, releaseBody string
	polls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached = append(reached, request.Method+" "+request.URL.Path)
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/machines":
			writeAPIData(t, writer, api.UserMachinePage{Items: []api.UserMachine{{ID: "machine_destination", EnvironmentID: "environment_destination", Alias: "destination", State: "ready", Online: true, Platform: "linux", InstallationGeneration: 1, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: true}}}}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case "POST /v1/sends/prepare":
			writeAPIData(t, writer, api.SendPrepareResponse{Request: api.TeamInboxRequest{RequestID: "request_pending", Status: "pending"}, FileTransferMode: "automatic"})
		case "GET /v1/team-inbox/requests/request_pending":
			polls++
			status := "pending"
			if polls > 1 {
				status = "approved"
			}
			writeAPIData(t, writer, api.TeamInboxRequest{RequestID: "request_pending", Status: status, ManifestDigest: "digest_pending"})
		case "POST /v1/machines/machine_destination/file-transfer-descriptor":
			body, _ := io.ReadAll(request.Body)
			descriptorBody = string(body)
			// A descriptor without an endpoint ends the send before any peer
			// work, pinning that the poll path fetched it with the binding.
			writeAPIData(t, writer, api.FileTransfer{SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Auth: api.AuthMaterial{Method: "bearer", Token: "transfer-token", AccessSessionID: "session_pending"}})
		case "POST /v1/file-transfer-sessions/release":
			body, _ := io.ReadAll(request.Body)
			releaseBody = string(body)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer backend.Close()

	_, configPath := localDaemonReadinessTestConfig(t, true)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "")
	file := filepath.Join(filepath.Dir(configPath), "pending.bin")
	if err := os.WriteFile(file, []byte("pending approval"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestProfile(t, filepath.Dir(configPath), configPath, backend.URL)
	prepareTestRegistration(t, backend.URL)
	server, _ := localDaemonReadinessServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer stopLocalDaemonReadinessServer(t, cancel, done)
	paths, err := hostDependenciesOf(t.Context()).daemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	waitForCommandSocket(t, paths.SocketPath)
	command := newRootCommand()
	var out, stderr bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--config", configPath, "--no-customization", "send", file, "--to", "destination"})
	commandCtx, finish := context.WithTimeout(context.Background(), 15*time.Second)
	defer finish()
	err = command.ExecuteContext(commandCtx)
	if err == nil || err.Error() != "server returned an invalid file transfer descriptor" {
		t.Fatalf("err=%v, want the descriptor validation", err)
	}
	for _, want := range []string{`"request_id":"request_pending"`, `"manifest_digest":"digest_pending"`, `"transfer_mode":"automatic"`} {
		if !strings.Contains(descriptorBody, want) {
			t.Fatalf("descriptor request lacks %s: %s", want, descriptorBody)
		}
	}
	if releaseBody != `{"access_session_ids":["session_pending"]}` {
		t.Fatalf("failed send released the wrong sessions: %q", releaseBody)
	}
	if polls < 2 {
		t.Fatalf("polls=%d, want pending then approved", polls)
	}
	for _, path := range reached {
		switch path {
		case "GET /v1/machines", "POST /v1/sends/prepare", "GET /v1/me/preferences", "GET /v1/team-inbox/requests/request_pending", "POST /v1/machines/machine_destination/file-transfer-descriptor", "POST /v1/file-transfer-sessions/release":
		default:
			t.Fatalf("pending send reached %s", path)
		}
	}
}
