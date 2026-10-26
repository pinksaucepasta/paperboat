package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/mcpserver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

func TestMCPSendDoesNotInheritTerminalDestination(t *testing.T) {
	_, configPath := localDaemonReadinessTestConfig(t, true)
	directory := filepath.Dir(configPath)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "inherited_terminal")
	file := filepath.Join(directory, "artifact")
	if err := os.WriteFile(file, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/machines" {
			machine := api.UserMachine{ID: "machine_remote", Alias: "remote", Online: true, State: "online"}
			writeAPIData(t, w, api.UserMachinePage{Items: []api.UserMachine{machine}, Pagination: api.Pagination{Limit: 200, Total: 1}})
			return
		}
		sends.Add(1)
		http.Error(w, "Inbox receive is not configured", http.StatusForbidden)
	}))
	defer server.Close()
	writeTestProfile(t, directory, configPath, server.URL)
	state, err := runtimeIdentityStore()
	if err != nil {
		t.Fatal(err)
	}
	registration, err := state.Registration()
	if err != nil {
		t.Fatal(err)
	}
	registration.ServerURL = server.URL
	if err := state.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}

	daemon, _ := localDaemonReadinessServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- daemon.Run(ctx) }()
	defer stopLocalDaemonReadinessServer(t, cancel, stopped)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	waitForCommandSocket(t, paths.SocketPath)
	root := newRootCommand()
	c, _, err := root.Find([]string{"mcp"})
	if err != nil || c.ParseFlags([]string{"--config", configPath}) != nil {
		t.Fatal("prepare MCP invocation")
	}
	b := &mcpBackend{command: c}
	if err := b.openRoots([]string{directory}); err != nil {
		t.Fatal(err)
	}
	defer b.closeRoots()
	_, err = b.SendFile(t.Context(), mcpserver.SendArgs{Machine: "remote", Path: file})
	var denied *api.APIError
	if !errors.As(err, &denied) || denied.Code != "machine_capability_unavailable" || sends.Load() != 0 {
		t.Fatalf("MCP inherited terminal authority and bypassed Inbox eligibility: sends=%d err=%v", sends.Load(), err)
	}
}

func TestMCPInventoryUsesBoundedAuthenticatedPage(t *testing.T) {
	_, configPath := localDaemonReadinessTestConfig(t, true)
	directory := filepath.Dir(configPath)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" || r.URL.Query().Get("limit") != "100" || r.URL.Query().Has("offset") || r.URL.Query().Has("cursor") || r.Header.Get("Authorization") == "" {
			t.Errorf("inventory did not use bounded account access: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected inventory request", http.StatusBadRequest)
			return
		}
		machines := make([]api.UserMachine, 100)
		for i := range machines {
			machines[i] = api.UserMachine{ID: fmt.Sprintf("machine_%d", i), Alias: "build", Online: true}
		}
		// Like the real server: one keyset page with the total and the next cursor.
		writeAPIData(t, w, map[string]any{"items": machines, "total": 101, "next_cursor": "after_machine_99"})
	}))
	defer server.Close()
	writeTestProfile(t, directory, configPath, server.URL)
	root := newRootCommand()
	c, _, err := root.Find([]string{"mcp"})
	if err != nil || c.ParseFlags([]string{"--config", configPath}) != nil {
		t.Fatal("prepare MCP invocation")
	}
	page, err := (&mcpBackend{command: c}).Machines(t.Context(), 100)
	if err != nil || len(page.Machines) != 100 || page.Total != 101 || !page.Truncated || page.Machines[0].ID != "machine_0" {
		t.Fatalf("MCP inventory lost its page contract: count=%d total=%d truncated=%v err=%v", len(page.Machines), page.Total, page.Truncated, err)
	}
}

func TestMCPExecOutputRetainsTerminalResultAfterTruncation(t *testing.T) {
	output := newMCPExecOutput()
	action := &command.Context{Reader: strings.NewReader(""), Writer: output, ErrWriter: io.Discard}
	encoder := json.NewEncoder(output)
	for _, event := range []tunnel.ExecEvent{
		{OperationID: "operation_1", State: "started"},
		{OperationID: "operation_1", Stream: "stdout", Data: bytes.Repeat([]byte("x"), mcpserver.MaxOutputBytes)},
		{OperationID: "operation_1", Stream: "stderr", Data: []byte("tail")},
		{OperationID: "operation_1", State: "exited", Result: &tunnel.ExecResult{Code: 42}},
	} {
		if err := writeExecEvent(action, encoder, event, false, true); err != nil {
			t.Fatal(err)
		}
	}
	result := output.finish(exitCodeError{42})
	if len(result.Stdout) != mcpserver.MaxOutputBytes || result.Stderr != "" || !result.Truncated {
		t.Fatalf("command output was not bounded: stdout=%d stderr=%d truncated=%v", len(result.Stdout), len(result.Stderr), result.Truncated)
	}
	if result.OperationID != "operation_1" || result.Outcome != "exited" || result.ExitCode == nil || *result.ExitCode != 42 {
		t.Fatal("truncation or nonzero exit lost the actual operation result")
	}
}

func TestMCPExecDoesNotHideFailureAfterKnownExit(t *testing.T) {
	output := newMCPExecOutput()
	action := &command.Context{Reader: strings.NewReader(""), Writer: output, ErrWriter: io.Discard}
	if err := writeExecEvent(action, json.NewEncoder(output), tunnel.ExecEvent{
		OperationID: "operation_1", State: "exited", Result: &tunnel.ExecResult{Code: 0},
	}, false, true); err != nil {
		t.Fatal(err)
	}
	result := output.finish(errors.New("fixture cleanup failure"))
	if result.ExitCode == nil || *result.ExitCode != 0 || result.ErrorCode == "" {
		t.Fatal("MCP hid the invocation failure behind a known successful process exit")
	}
}

func TestMCPExecPreservesNotStartedVersusUnknown(t *testing.T) {
	for _, test := range []struct {
		changed, uncertain bool
		code, outcome      string
	}{
		{false, false, "machine_offline", "not_started"},
		{true, true, "exec_start_uncertain", "unknown"},
	} {
		output := newMCPExecOutput()
		if err := writeExecJSONFailure(output, "operation_1", test.code, "safe detail", test.changed, test.uncertain); err != nil {
			t.Fatal(err)
		}
		result := output.finish(errors.New("private underlying error"))
		if result.OperationID != "operation_1" || result.ErrorCode != test.code || result.Outcome != test.outcome || result.Detail != "safe detail" {
			t.Fatalf("MCP lost the exec owner's outcome: %+v", result)
		}
	}
}

func TestExecRequiresInvocationInput(t *testing.T) {
	ctx := &command.Context{Context: t.Context(), Writer: io.Discard, ErrWriter: io.Discard}
	if err := actionRemoteExec(ctx, "build", tunnel.ExecRequest{Argv: []string{"cat"}}, true); err == nil {
		t.Fatal("exec accepted missing input instead of rejecting an unbound invocation")
	}
	root := newRootCommand()
	reader := strings.NewReader("explicit input")
	root.SetIn(reader)
	c, _, err := root.Find([]string{"exec"})
	if err != nil || c.ParseFlags(nil) != nil {
		t.Fatal("prepare execution invocation")
	}
	data, err := io.ReadAll(actionContext(c, nil).Reader)
	if err != nil || string(data) != "explicit input" {
		t.Fatal("invocation did not bind Cobra input to the exec owner")
	}
}

func TestMCPSendSnapshotsOnlyAllowedFiles(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(allowed, "artifact")
	if err := os.WriteFile(path, []byte("artifact bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &mcpBackend{}
	if err := b.openRoots([]string{allowed}); err != nil {
		t.Fatal(err)
	}
	defer b.closeRoots()
	snapshot, cleanup, err := b.prepareSend(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil || string(data) != "artifact bytes" || filepath.Base(snapshot) != "artifact" {
		t.Fatal("send action would reopen a mutable caller path instead of its private snapshot")
	}
	if _, _, err := b.prepareSend(t.Context(), filepath.Join(outside, "artifact")); err == nil {
		t.Fatal("outside path accepted")
	}
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("platform cannot create the external symlink")
	}
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.prepareSend(t.Context(), filepath.Join(link, "private")); err == nil {
		t.Fatal("symlink escaped the allowed root")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := b.prepareSend(canceled, path); err == nil {
		t.Fatal("canceled snapshot was accepted")
	}
}
