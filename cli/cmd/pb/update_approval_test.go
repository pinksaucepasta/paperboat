//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/cli/internal/liveconnections"
	"github.com/spf13/cobra"
)

const updateApprovalCandidateVersion = "2026.09.30.9"

var updateApprovalCandidateID = strings.Repeat("a", 64)

type updateApprovalControlFixture struct {
	listener *net.UnixListener
	socket   string
	done     chan struct{}
	mu       sync.Mutex
	requests []updated.ControlRequest
	pending  bool // answer as an install whose enrollment is not committed; guarded by mu
}

func (f *updateApprovalControlFixture) setPending() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = true
}

func (f *updateApprovalControlFixture) isPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

func startUpdateApprovalControlFixture(t *testing.T) *updateApprovalControlFixture {
	t.Helper()
	path := filepath.Join(shorttemp.Dir(t), "updater.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &updateApprovalControlFixture{listener: listener, done: make(chan struct{})}
	go fixture.serve()
	fixture.socket = updaterFixtureRootSocket(t, listener.Addr().String())
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
	} else if request.Operation == "check" {
		response.Pending = f.isPending()
	} else if f.isPending() {
		response = updated.ControlResponse{Schema: updated.ControlProtocolV1, Status: "error", ErrorCode: "installation_pending"}
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
	ctx := withUpdateDependencies(t.Context(), func(deps *updateDependencies) {
		deps.socket = func() (string, error) { return fixture.socket, nil }
		deps.inputIsTerminal = func(*cobra.Command) bool { return terminal }
	})

	root := newRootCommand()
	root.SetArgs(args)
	root.SetIn(strings.NewReader(input))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(ctx)
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

	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("force installs exact candidate without prompt json=%t", jsonOutput), func(t *testing.T) {
			fixture := startUpdateApprovalControlFixture(t)
			args := []string{"update", "--force"}
			if jsonOutput {
				args = append(args, "--json")
			}
			stdout, _, err := runUpdateApprovalCommand(t, fixture, args, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stdout, "[y/N]") {
				t.Fatal("force unexpectedly prompted")
			}
			install := expectedUpdateRequest("install", updateApprovalCandidateID)
			install.Force = true
			requireUpdateRequests(t, fixture, expectedUpdateRequest("download", ""), install)
		})
	}

	t.Run("malformed approval is rejected before updater access", func(t *testing.T) {
		fixture := startUpdateApprovalControlFixture(t)
		_, _, err := runUpdateApprovalCommand(t, fixture, []string{"update", "--approve", "not-a-candidate"}, "", false)
		// The refusal says what the ID must look like and what to run (it was the control client's generic invalid-request text).
		if err == nil || !strings.Contains(err.Error(), "64-character") || !strings.Contains(err.Error(), "pb update --approve") {
			t.Fatalf("malformed approval error = %v", err)
		}
		requireUpdateRequests(t, fixture)
	})
}

// Updating is fetching the signed public release, not an account feature: a check works before the machine is paired
// (prior art: `tailscale update` needs no login, ~/ref/tailscale/cmd/tailscale/cli/update.go).
func TestUpdateCheckWorksOnAnUnpairedInstall(t *testing.T) {
	oldVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	buildinfo.Version = "2026.09.01.1"
	fixture := startUpdateApprovalControlFixture(t)
	fixture.setPending()
	ctx := withUpdateDependencies(t.Context(), func(deps *updateDependencies) {
		deps.socket = func() (string, error) { return fixture.socket, nil }
	})
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"update", "check", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("unpaired check failed: %d %q %q", code, stdout.String(), stderr.String())
	}
	var envelope struct {
		Data updateCheckResult `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || envelope.Data.LatestVersion != updateApprovalCandidateVersion || !envelope.Data.UpdateAvailable || !envelope.Data.Verified {
		t.Fatalf("unpaired check result %+v (%v)", envelope, err)
	}
	requireUpdateRequests(t, fixture, expectedUpdateRequest("check", ""))
}

type pendingInstallRecord struct {
	calls              int
	executable, bundle string
	force              bool
	exportedTo         string
	exportedVersion    string // what the export reports; the candidate version unless a test changes it
}

// pendingUpdateContext runs `pb update` against an updater whose enrollment is pending, with the bundle export and the
// privileged supplied install replaced by recorders. The installed source reports the candidate's version.
func pendingUpdateContext(t *testing.T, fixture *updateApprovalControlFixture, installed *pendingInstallRecord, terminal bool) context.Context {
	t.Helper()
	installed.exportedVersion = updateApprovalCandidateVersion
	return withUpdateDependencies(t.Context(), func(deps *updateDependencies) {
		deps.socket = func() (string, error) { return fixture.socket, nil }
		deps.inputIsTerminal = func(*cobra.Command) bool { return terminal }
		deps.exportBundle = func(_ context.Context, _, output string) (string, error) {
			installed.exportedTo = output
			return installed.exportedVersion, os.WriteFile(output, []byte("bundle"), 0o600)
		}
		deps.openConnections = func(context.Context) (liveconnections.Counts, error) { return liveconnections.Counts{}, nil }
		deps.install = func(_ context.Context, executable string, _ installsource.Source, bundle string, force bool, _, _ string) (string, error) {
			installed.calls++
			installed.executable, installed.bundle, installed.force = executable, bundle, force
			return os.Executable()
		}
		deps.inspect = func(context.Context, string) (installsource.Source, error) {
			return installsource.Source{Version: updateApprovalCandidateVersion, Distribution: installsource.Official, AutomaticUpdates: true}, nil
		}
	})
}

func runPendingUpdate(t *testing.T, installed *pendingInstallRecord, terminal bool, input string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	oldVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	buildinfo.Version = "2026.09.01.1"
	fixture := startUpdateApprovalControlFixture(t)
	fixture.setPending()
	root := newRootCommand()
	root.SetArgs(append([]string{"update"}, args...))
	root.SetIn(strings.NewReader(input))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.ExecuteContext(pendingUpdateContext(t, fixture, installed, terminal))
	return out.String(), errOut.String(), err
}

// Updating is fetching the signed public release. While the enrollment is pending the updater's prepared update would
// replace the slot the enrollment commit verifies, so the signed bundle goes through the supplied-install transaction,
// which keeps the enrollment pending and re-points it at the new slot. It follows the approval rules of a downloaded
// candidate: a terminal asks first.
func TestUpdateOnAnUnpairedInstallInstallsTheSignedBundleAfterApproval(t *testing.T) {
	var installed pendingInstallRecord
	stdout, stderr, err := runPendingUpdate(t, &installed, true, "y\n")
	if err != nil {
		t.Fatalf("unpaired update failed: %v %q %q", err, stdout, stderr)
	}
	if installed.calls != 1 || installed.executable != "" || !strings.HasSuffix(installed.bundle, ".pb-update") || installed.force || installed.bundle != installed.exportedTo {
		t.Fatalf("supplied install = %+v", installed)
	}
	if want := "Updated Paperboat from 2026.09.01.1 to " + updateApprovalCandidateVersion + "."; !strings.Contains(stdout, want) || !strings.Contains(stdout, "Install Paperboat "+updateApprovalCandidateVersion+" now? [y/N]") {
		t.Fatalf("output %q lacks the approval prompt or %q", stdout, want)
	}
}

func TestUpdateOnAnUnpairedInstallDoesNotInstallWhenTheAnswerIsNo(t *testing.T) {
	var installed pendingInstallRecord
	stdout, _, err := runPendingUpdate(t, &installed, true, "n\n")
	if err != nil || installed.calls != 0 || installed.exportedTo != "" || !strings.Contains(stdout, "not installed") {
		t.Fatalf("declined unpaired update: err=%v installs=%d exported=%q %q", err, installed.calls, installed.exportedTo, stdout)
	}
}

// Without --force a JSON or unattended run only reports the release, as it does for a downloaded candidate.
func TestUpdateOnAnUnpairedInstallOnlyReportsWithoutForceWhenJSONOrUnattended(t *testing.T) {
	for name, tc := range map[string]struct {
		terminal bool
		args     []string
	}{
		"json on a terminal": {true, []string{"--json"}},
		"human, no terminal": {false, nil},
		"json, no terminal":  {false, []string{"--json"}},
	} {
		t.Run(name, func(t *testing.T) {
			var installed pendingInstallRecord
			stdout, stderr, err := runPendingUpdate(t, &installed, tc.terminal, "y\n", tc.args...)
			if err != nil || installed.calls != 0 || installed.exportedTo != "" || !strings.Contains(stdout, updateApprovalCandidateVersion) {
				t.Fatalf("unattended unpaired update: err=%v installs=%d exported=%q %q %q", err, installed.calls, installed.exportedTo, stdout, stderr)
			}
		})
	}
}

func TestUpdateForceOnAnUnpairedInstallInstallsWithoutAskingAndPassesForce(t *testing.T) {
	var installed pendingInstallRecord
	stdout, stderr, err := runPendingUpdate(t, &installed, false, "", "--json", "--force")
	if err != nil || installed.calls != 1 || !installed.force {
		t.Fatalf("forced unpaired update: err=%v %+v %q %q", err, installed, stdout, stderr)
	}
}

func TestUpdateOnAnUnpairedInstallThatIsCurrentInstallsNothing(t *testing.T) {
	var installed pendingInstallRecord
	oldVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	fixture := startUpdateApprovalControlFixture(t)
	fixture.setPending()
	buildinfo.Version = updateApprovalCandidateVersion
	root := newRootCommand()
	root.SetArgs([]string{"update", "--force"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.ExecuteContext(pendingUpdateContext(t, fixture, &installed, true)); err != nil || installed.calls != 0 || installed.exportedTo != "" || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("current unpaired update: err=%v installs=%d %q", err, installed.calls, out.String())
	}
}

// The install is bound to the release that was checked: a bundle of another version (the release moved on between the
// check and the export) installs nothing.
func TestUpdateOnAnUnpairedInstallInstallsNothingWhenTheBundleIsNotTheCheckedRelease(t *testing.T) {
	var installed pendingInstallRecord
	oldVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	buildinfo.Version = "2026.09.01.1"
	fixture := startUpdateApprovalControlFixture(t)
	fixture.setPending()
	ctx := pendingUpdateContext(t, fixture, &installed, false)
	installed.exportedVersion = "2026.09.15.3"
	root := newRootCommand()
	root.SetArgs([]string{"update", "--force"})
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	if err := root.ExecuteContext(ctx); err == nil || installed.calls != 0 {
		t.Fatalf("a bundle other than the checked release was installed: err=%v installs=%d", err, installed.calls)
	}
}

// The bundle is the signed release: an export that fails (bad signature, unreachable repository) installs nothing.
func TestUpdateOnAnUnpairedInstallInstallsNothingWhenTheSignedBundleCannotBeExported(t *testing.T) {
	var installed pendingInstallRecord
	oldVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	buildinfo.Version = "2026.09.01.1"
	fixture := startUpdateApprovalControlFixture(t)
	fixture.setPending()
	ctx := withUpdateDependencies(pendingUpdateContext(t, fixture, &installed, false), func(deps *updateDependencies) {
		deps.exportBundle = func(context.Context, string, string) (string, error) {
			return "", errors.New("signature verification failed")
		}
	})
	root := newRootCommand()
	root.SetArgs([]string{"update", "--force"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.ExecuteContext(ctx); err == nil || installed.calls != 0 {
		t.Fatalf("an unverifiable bundle was installed: err=%v installs=%d", err, installed.calls)
	}
}
