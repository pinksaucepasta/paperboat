//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"golang.org/x/sys/windows"
)

func TestWindowsUninstallPlanIsProtectedBoundedAndHelperIsNonRecursive(t *testing.T) {
	directory := t.TempDir()
	previousExecutable := windowsUninstallExecutable
	windowsUninstallExecutable = func() (string, error) { return filepath.Join(directory, "paperboat-uninstall-helper.exe"), nil }
	t.Cleanup(func() { windowsUninstallExecutable = previousExecutable })
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(directory, "plan.json")
	statusPath := filepath.Join(directory, "status.json")
	now := time.Now().UTC()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := windows.GetCurrentProcessToken().GetUserProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	plan := windowsUninstallPlan{
		Schema: windowsUninstallPlanSchema, SupportReference: supportref.New(), ProcessIDs: []uint32{uint32(os.Getpid()) + 100000},
		StatusPath: statusPath, InboxPaths: []string{filepath.Join(directory, "Paperboat Inbox")},
		CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute),
		OwnerSID: user.User.Sid.String(), OwnerProfile: profile, UserStatePaths: []string{filepath.Join(directory, "user-state")},
	}
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	loaded, err := readWindowsUninstallPlan(planPath)
	if err != nil || loaded.Schema != windowsUninstallPlanSchema || loaded.StatusPath != statusPath {
		t.Fatalf("loaded plan=%+v error=%v", loaded, err)
	}
	// Default state produced by an elevated CLI may have the Administrators
	// owner. It is still this user's Paperboat namespace, bound to the exact
	// token profile; an unrelated administrator directory is not authorized.
	adminState := filepath.Join(directory, "Paperboat")
	if err := os.Mkdir(adminState, 0700); err != nil {
		t.Fatal(err)
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(adminState, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, administrators, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	plan.UserStatePaths = []string{adminState}
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallPlan(planPath); err != nil {
		t.Fatalf("elevated Paperboat state in this profile was rejected: %v", err)
	}
	nestedState := filepath.Join(adminState, "runtime")
	if err := os.Mkdir(nestedState, 0700); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(nestedState, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, administrators, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	plan.UserStatePaths = []string{nestedState}
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallPlan(planPath); err != nil {
		t.Fatalf("nested elevated Paperboat state was rejected: %v", err)
	}
	unrelatedState := filepath.Join(directory, "unrelated-admin-state")
	if err := os.Rename(adminState, unrelatedState); err != nil {
		t.Fatal(err)
	}
	plan.UserStatePaths = []string{unrelatedState}
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallPlan(planPath); err == nil {
		t.Fatal("unrelated administrator-owned state was authorized")
	}
	plan.UserStatePaths = []string{filepath.Join(directory, "user-state")}
	plan.OwnerSID = "S-1-5-21-0-0-0-99999"
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallPlan(planPath); err == nil {
		t.Fatal("foreign-user cleanup plan accepted")
	}
	plan.OwnerSID = user.User.Sid.String()
	plan.UserStatePaths = []string{filepath.VolumeName(profile) + `\`}
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallPlan(planPath); err == nil {
		t.Fatal("filesystem-root cleanup plan accepted")
	}
	plan.UserStatePaths = []string{filepath.Join(directory, "user-state")}
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	windowsUninstallExecutable = func() (string, error) {
		return filepath.Join(directory, "other", "paperboat-uninstall-helper.exe"), nil
	}
	if _, err := readWindowsUninstallPlan(planPath); err == nil {
		t.Fatal("plan outside the running helper directory was accepted")
	}
	windowsUninstallExecutable = func() (string, error) { return filepath.Join(directory, "paperboat-uninstall-helper.exe"), nil }
	plan.CreatedAt = now.Add(-6 * time.Minute)
	plan.ExpiresAt = now.Add(-time.Minute)
	if err := writeProtectedWindowsUninstallJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallPlan(planPath); err == nil {
		t.Fatal("expired uninstall plan was accepted")
	}
	command := platformUninstallHelperCommand()
	if command.Name() != "__complete-uninstall" || !command.Hidden || strings.Contains(command.Use, "pb uninstall") || command.Flags().Lookup("plan") == nil {
		t.Fatalf("unsafe or recursive helper command: use=%q hidden=%v", command.Use, command.Hidden)
	}
	message := platformUninstallSuccessMessage()
	if !strings.Contains(message, "cleanup was started") || strings.Contains(strings.ToLower(message), "completely removed") {
		t.Fatalf("Windows uninstall status is not honest about asynchronous completion: %q", message)
	}
}

func TestWindowsUninstallHandoffAcceptsOnlyReadyProtectedStatus(t *testing.T) {
	directory := t.TempDir()
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(directory, "status.json")
	if err := writeProtectedWindowsUninstallJSON(statusPath, windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: "waiting_for_parent", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForWindowsUninstallHandoff(ctx, statusPath); err != nil {
		t.Fatalf("ready protected handoff was rejected: %v", err)
	}
	const canary = "PRIVATE uninstall cause"
	if err := writeProtectedWindowsUninstallJSON(statusPath, map[string]any{"schema": windowsUninstallStatusSchema, "state": "failed", "error": canary, "updated_at": time.Now().UTC(), "completed_at": time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := waitForWindowsUninstallHandoff(ctx, statusPath); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("unsafe failed handoff status: %v", err)
	}

	for _, invalid := range []windowsUninstallFailureMetadata{
		{Cause: "private_cause", SupportReference: supportref.New()},
		{Cause: "internal", Errno: 65536, SupportReference: supportref.New()},
		{Cause: "internal", SupportReference: "PRIVATE reference"},
		{Cause: "internal", SupportReference: supportref.New(), CleanupOperations: []string{"PRIVATE cleanup path"}},
		{Cause: "internal", SupportReference: supportref.New(), CleanupOperations: []string{"guard_task_remove", "guard_task_remove"}},
	} {
		value := windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: "failed", UpdatedAt: time.Now().UTC(), Failure: &invalid}
		if err := writeProtectedWindowsUninstallJSON(statusPath, value); err != nil {
			t.Fatal(err)
		}
		if err := waitForWindowsUninstallHandoff(ctx, statusPath); err == nil {
			t.Fatal("unbounded/private remote fault metadata accepted")
		}
	}
	// A process launch is not a handoff. A helper that exits during plan
	// validation must produce an immediate failure instead of stranding the
	// parent on a protected record that remains scheduled forever.
	if err := writeProtectedWindowsUninstallJSON(statusPath, windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: "scheduled", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	startup, stopStartup := context.WithTimeout(t.Context(), time.Second)
	defer stopStartup()
	if err := waitForWindowsUninstallHandoff(startup, statusPath); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing helper did not fail its handoff before the caller deadline: %v", err)
	}

}

// Exercise the protected wire and the ordinary final command owner together.
// No installed runtime or elevated uninstall is invoked by this fixture.
func TestWindowsUninstallFailureCrossProcessOwnershipAndRecovery(t *testing.T) {
	directory := t.TempDir()
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(directory, "status.json")
	reference := supportref.New()
	ctx := supportref.WithContext(t.Context(), reference)
	calls := 0
	original := &commandPrivateError{calls: &calls}
	local := windowsUninstallLocalFailure(reference, original, uninstallOperationTestError{machineguard.CleanupTaskRemove, windows.WSAETIMEDOUT}, uninstallOperationTestError{machineguard.CleanupWFPRemove, windows.WSAETIMEDOUT})
	if !errors.Is(local, original) || !errors.Is(local, windows.WSAETIMEDOUT) {
		t.Fatal("local original cause lost")
	}
	status := windowsUninstallStatusValue(ctx, "failed", local)
	if err := writeProtectedWindowsUninstallJSON(statusPath, status); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("PRIVATE_COMMAND_PAYLOAD")) || calls != 0 {
		t.Fatal("private original cause was formatted or serialized")
	}
	remote := waitForWindowsUninstallHandoff(t.Context(), statusPath)
	if observed, ok := remote.(windowsUninstallRemoteFailure); !ok || len(observed.metadata.CleanupOperations) != 2 || observed.metadata.CleanupOperations[0] != "guard_task_remove" || observed.metadata.CleanupOperations[1] != "guard_wfp_remove" {
		t.Fatal("protected status lost joined cleanup operation provenance")
	}
	if _, ok := remote.(windowsUninstallRemoteFailure); !ok {
		t.Fatalf("wire rejected producer: type=%T cause=%s errno=%d reference_valid=%t", remote, status.Failure.Cause, status.Failure.Errno, supportref.Valid(status.Failure.SupportReference))
	}
	if remote == nil || errors.Is(remote, windows.WSAETIMEDOUT) || errors.Is(remote, original) {
		t.Fatal("remote metadata became local syscall/original semantics")
	}
	restore := errorreport.Install(nil)
	defer restore()
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		if fault.Stage == "command" {
			faults = append(faults, fault)
		}
	})
	defer restoreObserver()
	old := tunnelClientForCommand
	defer func() { tunnelClientForCommand = old }()
	for index, failure := range []error{local, remote} {
		tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) { return nil, failure }
		var out, stderr bytes.Buffer
		if code := runWithReporter(t.Context(), []string{"--no-customization", "tunnel", "status", "tun_test", "--json"}, &out, &stderr, nil); code != 1 || stderr.Len() != 0 {
			t.Fatal("final command failure contract lost")
		}
		decoder := json.NewDecoder(&out)
		var result cliJSONEnvelope
		if err := decoder.Decode(&result); err != nil || result.Error == nil || result.Error.SupportReference != reference || result.Error.StateChanged != "unknown" {
			t.Fatalf("final result index=%d decode_failed=%t result_error=%+v expected_reference=%s fault_count=%d", index, err != nil, result.Error, reference, len(faults))
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF || bytes.Contains(out.Bytes(), []byte("PRIVATE_COMMAND_PAYLOAD")) {
			t.Fatal("duplicate/private final output")
		}
	}
	if len(faults) != 2 || faults[0].Code != "unexpected_cli_failure" || faults[1].Code != "command_failed" || faults[0].SupportReference != reference || faults[1].SupportReference != reference || faults[1].Errno != 10060 {
		t.Fatalf("final owner projection count=%d", len(faults))
	}
	if calls != 0 {
		t.Fatal("original private cause formatted")
	}
	if err := os.Remove(statusPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsUninstallStatus(statusPath); err == nil {
		t.Fatal("missing status lost original failure")
	} else {
		var native windows.Errno
		if !errors.As(err, &native) {
			t.Fatal("native status probe failure cause lost")
		}
	}
	if err := writeProtectedWindowsUninstallJSON(statusPath, windowsUninstallStatusValue(ctx, "completed", nil)); err != nil {
		t.Fatal(err)
	}
	if err := waitForWindowsUninstallHandoff(t.Context(), statusPath); err != nil {
		t.Fatal("fresh protected completion did not recover")
	}
}

func TestWindowsUninstallRequiresConfirmedDaemonStopBeforeProductRemoval(t *testing.T) {
	if !platformRequiresConfirmedDaemonStop() {
		t.Fatal("Windows uninstall may remove product/state after uncertain daemon termination")
	}
}

func TestWindowsUninstallSchedulesCompleteHelperDirectoryDeletion(t *testing.T) {
	previous := moveWindowsFileAtReboot
	var deleted []string
	moveWindowsFileAtReboot = func(path *uint16) error {
		deleted = append(deleted, windows.UTF16PtrToString(path))
		return nil
	}
	t.Cleanup(func() { moveWindowsFileAtReboot = previous })
	scheduleWindowsUninstallDirectoryDeletionObserved(t.Context(), `C:\unrelated`)
	if len(deleted) != 0 {
		t.Fatalf("unsafe helper directory scheduled deletions: %v", deleted)
	}
	directory := filepath.Join(os.TempDir(), "Paperboat Uninstall", "0123456789abcdef0123456789abcdef")
	scheduleWindowsUninstallDirectoryDeletionObserved(t.Context(), directory)
	want := []string{
		filepath.Join(directory, "plan.json"),
		filepath.Join(directory, "status.json"),
		filepath.Join(directory, "paperboat-uninstall-helper.exe"),
		directory,
	}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("scheduled helper deletions=%v want=%v", deleted, want)
	}
}

func TestRecoverExpiredWindowsUninstallHelpersRemovesOnlyValidatedInactiveRecords(t *testing.T) {
	previousRunning := windowsProcessIsRunning
	previousHelperRunning := windowsHelperIsRunning
	previousRoot := windowsUninstallRoot
	testRoot := filepath.Join(t.TempDir(), "Paperboat Uninstall")
	windowsUninstallRoot = func() string { return testRoot }
	windowsProcessIsRunning = func(processID uint32) (bool, error) {
		if processID != 4242 {
			t.Fatalf("unexpected process ID %d", processID)
		}
		return false, nil
	}
	windowsHelperIsRunning = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() {
		windowsProcessIsRunning = previousRunning
		windowsHelperIsRunning = previousHelperRunning
		windowsUninstallRoot = previousRoot
	})
	root := windowsUninstallRoot()
	directory := filepath.Join(root, "0123456789abcdef0123456789abcdef")
	_ = os.RemoveAll(root)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-10 * time.Minute)
	statusPath := filepath.Join(directory, "status.json")
	plan := windowsUninstallPlan{Schema: windowsUninstallPlanSchema, SupportReference: supportref.New(), ProcessIDs: []uint32{4242}, StatusPath: statusPath, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err := writeProtectedWindowsUninstallJSON(filepath.Join(directory, "plan.json"), plan); err != nil {
		t.Fatal(err)
	}
	if err := writeProtectedWindowsUninstallJSON(statusPath, windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: "failed", UpdatedAt: now, CompletedAt: now}); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(directory, "paperboat-uninstall-helper.exe")
	if err := os.WriteFile(helper, []byte("old helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsUninstallPath(helper, false); err != nil {
		t.Fatal(err)
	}
	if err := recoverExpiredWindowsUninstallHelpers(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired helper remains: %v", err)
	}
}

func TestRecoverExpiredWindowsUninstallHelpersRefusesActiveOrMalformedRecords(t *testing.T) {
	previousRunning := windowsProcessIsRunning
	previousHelperRunning := windowsHelperIsRunning
	previousRoot := windowsUninstallRoot
	testRoot := filepath.Join(t.TempDir(), "Paperboat Uninstall")
	windowsUninstallRoot = func() string { return testRoot }
	windowsHelperIsRunning = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() {
		windowsProcessIsRunning = previousRunning
		windowsHelperIsRunning = previousHelperRunning
		windowsUninstallRoot = previousRoot
	})
	root := windowsUninstallRoot()
	_ = os.RemoveAll(root)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	directory := filepath.Join(root, "fedcba9876543210fedcba9876543210")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-10 * time.Minute)
	statusPath := filepath.Join(directory, "status.json")
	plan := windowsUninstallPlan{Schema: windowsUninstallPlanSchema, SupportReference: supportref.New(), ProcessIDs: []uint32{4343}, StatusPath: statusPath, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err := writeProtectedWindowsUninstallJSON(filepath.Join(directory, "plan.json"), plan); err != nil {
		t.Fatal(err)
	}
	if err := writeProtectedWindowsUninstallJSON(statusPath, windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: "failed", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(directory, "paperboat-uninstall-helper.exe")
	if err := os.WriteFile(helper, []byte("old helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsUninstallPath(helper, false); err != nil {
		t.Fatal(err)
	}
	windowsProcessIsRunning = func(uint32) (bool, error) { return true, nil }
	if err := recoverExpiredWindowsUninstallHelpers(); err != nil {
		t.Fatalf("active helper blocked safe recovery scan: %v", err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("active helper was removed: %v", err)
	}
	windowsProcessIsRunning = func(uint32) (bool, error) { return false, nil }
	if err := os.WriteFile(filepath.Join(directory, "unexpected"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverExpiredWindowsUninstallHelpers(); err != nil {
		t.Fatalf("malformed helper blocked safe recovery scan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "unexpected")); err != nil {
		t.Fatalf("malformed helper record was mutated: %v", err)
	}
}

func TestWindowsInstallUsesRunningBytesAndFixedLayout(t *testing.T) {
	previousSource, previousInstall := runningInstallSource, installSuppliedExecutable
	source := installsource.Source{Version: "dev", Platform: "windows", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Length: 42, Distribution: installsource.Custom}
	runningInstallSource = func() (string, installsource.Source, error) { return `C:\staged\pb.exe`, source, nil }
	called := false
	installSuppliedExecutable = func(_ context.Context, path string, got installsource.Source, directory, _ string) (string, error) {
		called = true
		if path != `C:\staged\pb.exe` || got != source || directory != "" {
			t.Fatalf("install request path=%q source=%+v directory=%q", path, got, directory)
		}
		return `C:\Program Files\Paperboat\users\owner\bin\pb.exe`, nil
	}
	t.Cleanup(func() { runningInstallSource, installSuppliedExecutable = previousSource, previousInstall })
	command := platformInstallCommand()
	command.SetArgs([]string{"--json"})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.ExecuteContext(context.Background()); err != nil || !called || !strings.Contains(output.String(), `"enrollment":"unchanged"`) {
		t.Fatalf("called=%t output=%q error=%v", called, output.String(), err)
	}
}

func TestCopyWindowsUninstallHelperVerifiesExactBytes(t *testing.T) {
	directory := t.TempDir()
	if err := protectWindowsUninstallPath(directory, true); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "source.exe")
	want := []byte("paperboat helper test bytes")
	if err := os.WriteFile(source, want, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "helper.exe")
	if err := copyWindowsUninstallHelper(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("copied helper bytes=%q error=%v", got, err)
	}
}

func TestRemovePathPreservingIsCaseInsensitiveOnWindows(t *testing.T) {
	root := t.TempDir()
	inboxPath := filepath.Join(root, "Nested", "Paperboat Inbox")
	if err := os.MkdirAll(inboxPath, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(inboxPath, "Keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "remove.txt"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePathPreserving(strings.ToUpper(root), []string{strings.ToLower(inboxPath)}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep" {
		t.Fatalf("case-insensitive Inbox preservation content=%q error=%v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-Inbox state remains: %v", err)
	}
}

func TestFilesystemRootRejectsWindowsDriveAndUNCShare(t *testing.T) {
	if !filesystemRoot(`C:\`) || !filesystemRoot(`\\server\share\`) || filesystemRoot(`C:\Paperboat`) {
		t.Fatal("Windows filesystem-root detection accepted a destructive cleanup target")
	}
}

var commandRuntimeTestSequence uint64

func commandLocalAPIServerConfig(path string, source localapi.SnapshotSource) (localapi.ServerConfig, error) {
	sid, err := localdaemon.CurrentUserSID()
	if err != nil {
		return localapi.ServerConfig{}, err
	}
	return localapi.ServerConfig{SocketPath: path, OwnerUID: -1, OwnerGID: -1, OwnerSID: sid, Source: source}, nil
}

func commandRuntimeTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sequence := atomic.AddUint64(&commandRuntimeTestSequence, 1)
	stateRoot := filepath.Join(root, "state")
	runtimeRoot := filepath.Join(root, "runtime")
	// Named pipes are global on Windows. The process ID prevents a retry from
	// colliding with a pipe retained by an earlier test process, while the
	// sequence keeps roots distinct inside this process.
	socketPath := `\\.\pipe\paperboat-test-` + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(sequence, 10)
	previousPaths := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) {
		return localapi.Paths{
			StateRoot: stateRoot, RuntimeRoot: runtimeRoot, SocketPath: socketPath,
			LockPath: filepath.Join(stateRoot, "daemon.lock"),
		}, nil
	}
	t.Cleanup(func() { currentLocalDaemonPaths = previousPaths })
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("APPDATA", root)
	previous := []string{xdg.Home, xdg.ConfigHome, xdg.CacheHome, xdg.DataHome, xdg.StateHome, xdg.RuntimeDir}
	xdg.Home = root
	xdg.ConfigHome = root
	xdg.CacheHome = root
	xdg.DataHome = root
	xdg.StateHome = root
	xdg.RuntimeDir = root
	t.Cleanup(func() {
		xdg.Home, xdg.ConfigHome, xdg.CacheHome, xdg.DataHome, xdg.StateHome, xdg.RuntimeDir = previous[0], previous[1], previous[2], previous[3], previous[4], previous[5]
	})
	return root
}

func TestCommandRuntimeTestRootUsesProcessScopedNamedPipes(t *testing.T) {
	firstRoot := commandRuntimeTestRoot(t)
	first, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	secondRoot := commandRuntimeTestRoot(t)
	second, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	prefix := `\\.\pipe\paperboat-test-` + strconv.Itoa(os.Getpid()) + "-"
	if !strings.HasPrefix(first.SocketPath, prefix) || !strings.HasPrefix(second.SocketPath, prefix) {
		t.Fatalf("pipes must include the test process ID: first=%q second=%q", first.SocketPath, second.SocketPath)
	}
	if first.SocketPath == second.SocketPath || first.RuntimeRoot == second.RuntimeRoot || firstRoot == secondRoot {
		t.Fatalf("test roots must be distinct: first=%#v second=%#v", first, second)
	}
}

func isolateCommandCredentialLocation(t *testing.T, root string) {
	t.Helper()
	previous := xdg.ConfigHome
	xdg.ConfigHome = root
	t.Cleanup(func() { xdg.ConfigHome = previous })
}

func waitForCommandSocket(t *testing.T, path string) {
	t.Helper()
	client, err := localapi.NewClient(path, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err = client.Snapshot(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("local command pipe %s was not ready: %v", path, err)
}
