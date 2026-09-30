//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/spf13/cobra"
)

const updateApprovalCandidateVersion = "2026.09.30.9"

var updateApprovalCandidateID = strings.Repeat("a", 64)

type updateApprovalControlFixture struct {
	listener *net.UnixListener
	done     chan struct{}
	mu       sync.Mutex
	requests []updated.ControlRequest
}

func startUpdateApprovalControlFixture(t *testing.T) *updateApprovalControlFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "updater.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &updateApprovalControlFixture{listener: listener, done: make(chan struct{})}
	go fixture.serve()
	t.Cleanup(func() {
		_ = fixture.listener.Close()
		select {
		case <-fixture.done:
		case <-time.After(time.Second):
			t.Error("local updater fixture did not stop")
		}
	})
	return fixture
}

func (f *updateApprovalControlFixture) serve() {
	defer close(f.done)
	for {
		connection, err := f.listener.AcceptUnix()
		if err != nil {
			return
		}
		f.handle(connection)
	}
}

func (f *updateApprovalControlFixture) handle(connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	var request updated.ControlRequest
	decoder := json.NewDecoder(connection)
	decodeErr := decoder.Decode(&request)
	if decodeErr == nil {
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			decodeErr = errors.New("updater request contained trailing data")
		}
	}
	if decodeErr == nil {
		f.mu.Lock()
		f.requests = append(f.requests, request)
		f.mu.Unlock()
	}

	response := updated.ControlResponse{Schema: updated.ControlProtocolV1, Status: "ok", Version: updateApprovalCandidateVersion}
	if decodeErr != nil {
		response = updated.ControlResponse{Schema: updated.ControlProtocolV1, Status: "error", ErrorCode: "invalid_request", ErrorMessage: "invalid local test request"}
	} else if request.Operation == "download" {
		response.Candidate = &workerupdate.PreparedCandidate{
			ID: updateApprovalCandidateID, Version: updateApprovalCandidateVersion,
			Platform: "linux", Architecture: "amd64", SHA256: strings.Repeat("b", 64), Length: 1234,
		}
	} else if request.Operation == "install" {
		response.Updated = true
		response.Pending = true
	}
	_ = json.NewEncoder(connection).Encode(response)
	_ = connection.CloseWrite()
}

func (f *updateApprovalControlFixture) recordedRequests() []updated.ControlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]updated.ControlRequest(nil), f.requests...)
}

func runUpdateApprovalCommand(t *testing.T, fixture *updateApprovalControlFixture, args []string, input string, terminal bool) (string, string, error) {
	t.Helper()
	previousSocket := updateControlSocketForCommand
	previousTerminal := updateInputIsTerminal
	updateControlSocketForCommand = func() (string, error) { return fixture.listener.Addr().String(), nil }
	updateInputIsTerminal = func(*cobra.Command) bool { return terminal }
	t.Cleanup(func() {
		updateControlSocketForCommand = previousSocket
		updateInputIsTerminal = previousTerminal
	})

	root := newRootCommand()
	root.SetArgs(args)
	root.SetIn(strings.NewReader(input))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func requireUpdateRequests(t *testing.T, fixture *updateApprovalControlFixture, want ...updated.ControlRequest) {
	t.Helper()
	got := fixture.recordedRequests()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("updater requests = %#v, want %#v", got, want)
	}
}

func expectedUpdateRequest(operation, approvalID string) updated.ControlRequest {
	return updated.ControlRequest{Schema: updated.ControlProtocolV1, Operation: operation, ApprovalID: approvalID}
}

func TestUpdateApprovalFlowStagesAndInstallsOnlyTheReviewedCandidate(t *testing.T) {
	t.Run("download subcommand stages without installing", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		stdout, _, err := runUpdateApprovalCommand(t, fixture, []string{"update", "download"}, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, updateApprovalCandidateID) || !strings.Contains(stdout, "Review this update") {
			t.Fatalf("download output did not identify the reviewable candidate: %q", stdout)
		}
		requireUpdateRequests(t, fixture, expectedUpdateRequest("download", ""))
	})

	t.Run("non-terminal update stages without installing", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		stdout, _, err := runUpdateApprovalCommand(t, fixture, []string{"update"}, "y\n", false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, updateApprovalCandidateID) || strings.Contains(stdout, "Install Paperboat") {
			t.Fatalf("non-terminal update did not leave the candidate staged: %q", stdout)
		}
		requireUpdateRequests(t, fixture, expectedUpdateRequest("download", ""))
	})

	t.Run("JSON update stages without prompting or installing", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		stdout, _, err := runUpdateApprovalCommand(t, fixture, []string{"update", "--json"}, "y\n", true)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Data updated.ControlResponse `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("JSON update output = %q: %v", stdout, err)
		}
		if envelope.Data.Candidate == nil || envelope.Data.Candidate.ID != updateApprovalCandidateID {
			t.Fatalf("JSON output did not return the exact staged candidate: %+v", envelope.Data)
		}
		requireUpdateRequests(t, fixture, expectedUpdateRequest("download", ""))
	})

	t.Run("empty interactive answer declines installation", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		stdout, _, err := runUpdateApprovalCommand(t, fixture, []string{"update"}, "\n", true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "installation was not requested") {
			t.Fatalf("default decline was not reported: %q", stdout)
		}
		requireUpdateRequests(t, fixture, expectedUpdateRequest("download", ""))
	})

	t.Run("interactive confirmation installs the exact candidate", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		stdout, _, err := runUpdateApprovalCommand(t, fixture, []string{"update"}, "y\n", true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "is staged") {
			t.Fatalf("installation response was not rendered: %q", stdout)
		}
		requireUpdateRequests(t, fixture,
			expectedUpdateRequest("download", ""),
			expectedUpdateRequest("install", updateApprovalCandidateID),
		)
	})

	t.Run("explicit approval installs without downloading again", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		stdout, _, err := runUpdateApprovalCommand(t, fixture, []string{"update", "--approve", updateApprovalCandidateID}, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "is staged") {
			t.Fatalf("approved installation response was not rendered: %q", stdout)
		}
		requireUpdateRequests(t, fixture, expectedUpdateRequest("install", updateApprovalCandidateID))
	})

	t.Run("malformed approval is rejected before updater access", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		_, _, err := runUpdateApprovalCommand(t, fixture, []string{"update", "--approve", "not-a-candidate"}, "", false)
		if err == nil || !strings.Contains(err.Error(), "invalid paperboat-updated control request") {
			t.Fatalf("malformed approval error = %v", err)
		}
		requireUpdateRequests(t, fixture)
	})
}
