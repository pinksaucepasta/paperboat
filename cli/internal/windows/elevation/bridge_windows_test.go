//go:build windows

package elevation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCreateProcessForRunRequestsBreakawayFromParentJob(t *testing.T) {
	flags := createProcessFlagsForRun()
	if flags&windows.CREATE_BREAKAWAY_FROM_JOB == 0 {
		t.Fatal("detached elevated processes must explicitly break away from the caller job")
	}
	for _, required := range []uint32{windows.CREATE_NO_WINDOW, windows.CREATE_NEW_PROCESS_GROUP} {
		if flags&required == 0 {
			t.Fatalf("detached process flags=%#x do not include %#x", flags, required)
		}
	}
}

func assertRenameBlocked(t *testing.T, from, to string) {
	t.Helper()
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		t.Fatal(err)
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		t.Fatal(err)
	}
	err = windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_WRITE_THROUGH)
	if err == nil {
		t.Fatalf("rename %q to %q succeeded while a bridge handle was open", from, to)
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("rename %q to %q error = %v, want sharing violation or access denied", from, to, err)
	}
}

func createDirectoryReparseSwap(t *testing.T, original, target string) {
	t.Helper()
	linkPtr, err := windows.UTF16PtrFromString(original)
	if err != nil {
		t.Fatal(err)
	}
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	err = windows.CreateSymbolicLink(linkPtr, targetPtr, windows.SYMBOLIC_LINK_FLAG_DIRECTORY)
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("reparse-point creation is unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(original) })
}

func TestBridgeParametersContainOnlyProtectedFilePaths(t *testing.T) {
	paths := bridgePaths{
		request: "C:\\Users\\Test User\\AppData\\Local\\Paperboat\\elevation\\request-abc\\request.json",
		result:  "C:\\Users\\Test User\\AppData\\Local\\Paperboat\\elevation\\request-abc\\result.json",
		cancel:  "C:\\Users\\Test User\\AppData\\Local\\Paperboat\\elevation\\request-abc\\cancel",
	}
	parameters := bridgeParameters(paths)
	for _, expected := range []string{"__runtime-service", "--request-file", "request.json", "--result-file", "result.json", "--cancel-file", "cancel"} {
		if !strings.Contains(parameters, expected) {
			t.Fatalf("parameters %q do not contain %q", parameters, expected)
		}
	}
	if strings.Contains(parameters, "enrollment-token") || strings.Contains(parameters, "private-key") || strings.Contains(parameters, "secret-value") {
		t.Fatalf("elevation parameters contain secret material: %q", parameters)
	}
}

func TestProtectedBridgeFilesUseCurrentUserACL(t *testing.T) {
	ownerSID, err := currentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := newBridgeFiles(ownerSID)
	if err != nil {
		if os.Getenv("LOCALAPPDATA") == "" {
			t.Skip("LOCALAPPDATA is unavailable")
		}
		t.Fatal(err)
	}
	defer cleanup()
	if err := verifyProtectedDirectory(paths.directory, ownerSID); err != nil {
		t.Fatalf("request directory ACL: %v", err)
	}
	request := Request{Schema: SchemaV1, RequestID: "request-test", OwnerSID: ownerSID, Operation: OperationOpenSSH, Action: ActionOpenSSHSetup, CancelPath: paths.cancel, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
		t.Fatal(err)
	}
	if err := verifyProtectedFile(paths.request, ownerSID, true); err != nil {
		t.Fatalf("request file ACL: %v", err)
	}
	decoded, err := readProtectedRequest(paths.request)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RequestID != request.RequestID || decoded.OwnerSID != ownerSID {
		t.Fatalf("decoded request = %#v", decoded)
	}
}

func TestOpenBridgeFileRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "request.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	linkPtr, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CreateHardLink(linkPtr, targetPtr, 0); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			t.Skipf("hard links are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := openBridgeFile(link, windows.FILE_GENERIC_READ); err == nil {
		t.Fatal("openBridgeFile accepted a hard-linked bridge file")
	} else if !strings.Contains(strings.ToLower(err.Error()), "hard link") {
		t.Fatalf("openBridgeFile error = %v, want hard-link rejection", err)
	}
}

func TestOpenBridgeFilePreventsRenameSwap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "request.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := openBridgeFile(path, windows.FILE_GENERIC_READ)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)

	assertRenameBlocked(t, path, filepath.Join(root, "request.swap"))
	if _, err := bridgeHandleSize(handle); err != nil {
		t.Fatalf("validated handle became unusable after blocked swap: %v", err)
	}
}

func TestOpenBridgeHandlesPreventDirectoryRenameSwap(t *testing.T) {
	ownerSID, err := currentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := newBridgeFiles(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	request := Request{
		Schema:     SchemaV1,
		RequestID:  filepath.Base(paths.directory),
		OwnerSID:   ownerSID,
		Operation:  OperationOpenSSH,
		Action:     ActionOpenSSHSetup,
		CancelPath: paths.cancel,
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(time.Minute),
	}
	if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
		t.Fatal(err)
	}
	handles, err := openBridgeHandles(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer handles.close()

	assertRenameBlocked(t, paths.directory, paths.directory+".swap")
}

func TestOpenBridgeDirectoryChainRejectsReparseSwap(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "bridge")
	backup := filepath.Join(root, "bridge.original")
	target := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(original, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, backup); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(original)
		_ = os.Rename(backup, original)
	})
	createDirectoryReparseSwap(t, original, target)

	if _, _, err := openBridgeDirectoryChain(filepath.Join(original, "child"), windows.FILE_GENERIC_READ); err == nil {
		t.Fatal("openBridgeDirectoryChain followed a reparse-point directory")
	} else if !strings.Contains(strings.ToLower(err.Error()), "reparse") {
		t.Fatalf("openBridgeDirectoryChain error = %v, want reparse rejection", err)
	}
}

func TestRunOpenSSHMapsUACCancellationToTypedError(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bridgeTestMutex.Lock()
	defer bridgeTestMutex.Unlock()
	previous := shellExecuteExForRun
	previousElevated := isCurrentProcessElevatedForRun
	shellExecuteExForRun = func(string, string, string) (windows.Handle, error) {
		return 0, windows.ERROR_CANCELLED
	}
	isCurrentProcessElevatedForRun = func() bool { return false }
	defer func() {
		shellExecuteExForRun = previous
		isCurrentProcessElevatedForRun = previousElevated
	}()
	err = RunOpenSSH(context.Background(), executable, ActionOpenSSHSetup)
	if !errors.Is(err, ErrElevationDenied) {
		t.Fatalf("error = %v, want ErrElevationDenied", err)
	}
}

func TestPinnedExecutableRejectsReplacementAndHardLinks(t *testing.T) {
	source := filepath.Join(t.TempDir(), "paperboat.exe")
	if err := os.WriteFile(source, []byte("MZ test"), 0o600); err != nil {
		t.Fatal(err)
	}
	validated, handle, err := pinExecutable(source)
	if err != nil {
		t.Fatal(err)
	}
	if validated != source {
		t.Fatalf("validated executable = %q, want %q", validated, source)
	}
	replacement := filepath.Join(filepath.Dir(source), "replacement.exe")
	if err := os.Rename(source, replacement); err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("pinned executable was renamed while its validation handle was held")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, replacement); err != nil {
		t.Fatalf("executable remained pinned after close: %v", err)
	}
	link := filepath.Join(filepath.Dir(source), "paperboat-link.exe")
	if err := os.Link(replacement, link); err != nil {
		t.Skipf("hard links are unavailable on this test volume: %v", err)
	}
	if _, handle, err := pinExecutable(replacement); err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("multiply-linked elevation executable was accepted")
	}
}

func TestRunOperationAllowsCanonicalRestoreWhileHelperRemainsPinned(t *testing.T) {
	source := filepath.Join(t.TempDir(), "paperboat.exe")
	want := []byte("MZ original approved helper bytes")
	if err := os.WriteFile(source, want, 0o600); err != nil {
		t.Fatal(err)
	}
	bridgeTestMutex.Lock()
	defer bridgeTestMutex.Unlock()
	previous, previousElevated := shellExecuteExForRun, isCurrentProcessElevatedForRun
	defer func() { shellExecuteExForRun, isCurrentProcessElevatedForRun = previous, previousElevated }()
	var observed error
	var helper string
	shellExecuteExForRun = func(executable, _, _ string) (windows.Handle, error) {
		helper = executable
		rollback := source + ".rollback"
		if err := os.Rename(source, rollback); err != nil {
			observed = fmt.Errorf("canonical restoration was blocked by its elevation launcher: %w", err)
			return 0, windows.ERROR_CANCELLED
		}
		if err := os.Rename(rollback, source); err != nil {
			observed = err
			return 0, windows.ERROR_CANCELLED
		}
		body, err := os.ReadFile(executable)
		if err != nil || !bytes.Equal(body, want) {
			observed = fmt.Errorf("launched helper differs from approved source: %v", err)
			return 0, windows.ERROR_CANCELLED
		}
		if err := os.Rename(executable, executable+".replacement"); err == nil {
			observed = errors.New("validated helper could be replaced during launch")
			return 0, windows.ERROR_CANCELLED
		}
		if err := os.WriteFile(executable, []byte("MZ substituted"), 0o600); err == nil {
			observed = errors.New("validated helper could be modified during launch")
			return 0, windows.ERROR_CANCELLED
		}
		if err := os.Rename(filepath.Dir(executable), filepath.Dir(executable)+".replacement"); err == nil {
			observed = errors.New("validated helper directory could be replaced during launch")
		}
		return 0, windows.ERROR_CANCELLED
	}
	isCurrentProcessElevatedForRun = func() bool { return false }
	err := RunRuntimeService(context.Background(), source, ActionRepair, nil)
	if observed != nil {
		t.Fatal(observed)
	}
	if !errors.Is(err, ErrElevationDenied) {
		t.Fatalf("error = %v, want typed canceled elevation", err)
	}
	if helper == "" || strings.EqualFold(helper, source) {
		t.Fatal("native mutation was launched from the canonical slot")
	}
	if _, err := os.Stat(filepath.Dir(helper)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed helper operation was not cleaned: %v", err)
	}
}

func TestRunOperationRejectsReparseExecutableBeforeLaunch(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.exe")
	if err := os.WriteFile(source, []byte("MZ unapproved alias target"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias.exe")
	aliasPtr, _ := windows.UTF16PtrFromString(alias)
	sourcePtr, _ := windows.UTF16PtrFromString(source)
	if err := windows.CreateSymbolicLink(aliasPtr, sourcePtr, 0); err != nil {
		t.Fatal(err)
	}
	bridgeTestMutex.Lock()
	defer bridgeTestMutex.Unlock()
	previous, previousElevated := shellExecuteExForRun, isCurrentProcessElevatedForRun
	defer func() { shellExecuteExForRun, isCurrentProcessElevatedForRun = previous, previousElevated }()
	launched := false
	shellExecuteExForRun = func(string, string, string) (windows.Handle, error) {
		launched = true
		return 0, windows.ERROR_CANCELLED
	}
	isCurrentProcessElevatedForRun = func() bool { return false }
	if err := RunRuntimeService(context.Background(), alias, ActionRepair, nil); !errors.Is(err, ErrElevationUnavailable) {
		t.Fatalf("reparse source error = %v, want invalid executable", err)
	}
	if launched {
		t.Fatal("a reparse source reached the privileged launcher")
	}
}

func TestRunOperationRetainsPinnedHelperUntilLateLaunchCompletes(t *testing.T) {
	source := filepath.Join(t.TempDir(), "paperboat.exe")
	if err := os.WriteFile(source, []byte("MZ approved late helper"), 0o600); err != nil {
		t.Fatal(err)
	}
	bridgeTestMutex.Lock()
	defer bridgeTestMutex.Unlock()
	previous, previousElevated := shellExecuteExForRun, isCurrentProcessElevatedForRun
	defer func() { shellExecuteExForRun, isCurrentProcessElevatedForRun = previous, previousElevated }()
	launched := make(chan string, 1)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	shellExecuteExForRun = func(executable, _, _ string) (windows.Handle, error) {
		launched <- executable
		<-release
		return 0, windows.ERROR_CANCELLED
	}
	isCurrentProcessElevatedForRun = func() bool { return false }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunRuntimeService(ctx, source, ActionRepair, nil) }()
	var helper string
	select {
	case helper = <-launched:
	case <-time.After(5 * time.Second):
		t.Fatal("helper launch did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrElevationCanceled) {
			t.Fatalf("canceled operation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller did not return after the bounded launch grace")
	}
	if err := os.WriteFile(helper, []byte("MZ substituted after caller exit"), 0o600); err == nil {
		t.Fatal("pending launcher lost its validated helper pin when the caller exited")
	}
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("pending launch files were retired before completion: %v", err)
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Dir(helper)); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed late helper was not retired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBridgeRejectsHardLinkedResultPlaceholder(t *testing.T) {
	ownerSID, err := currentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := newBridgeFiles(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	request := Request{Schema: SchemaV1, RequestID: filepath.Base(paths.directory), OwnerSID: ownerSID, Operation: OperationOpenSSH, Action: ActionOpenSSHSetup, CancelPath: paths.cancel, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.directory, "result-link.json")
	if err := os.Link(paths.result, link); err != nil {
		t.Skipf("hard links are unavailable on this test volume: %v", err)
	}
	if handles, err := openBridgeHandles(paths); err == nil {
		_ = handles.close()
		t.Fatal("hard-linked elevation result placeholder was accepted")
	}
}

func TestBridgeRejectsHardLinkedResultSwap(t *testing.T) {
	ownerSID, err := currentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := newBridgeFiles(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	request := Request{Schema: SchemaV1, RequestID: filepath.Base(paths.directory), OwnerSID: ownerSID, Operation: OperationOpenSSH, Action: ActionOpenSSHSetup, CancelPath: paths.cancel, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(paths.directory, "sentinel")
	if err := os.WriteFile(sentinel, []byte("sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.result); err != nil {
		t.Fatal(err)
	}
	sentinelPtr, err := windows.UTF16PtrFromString(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	resultPtr, err := windows.UTF16PtrFromString(paths.result)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CreateHardLink(resultPtr, sentinelPtr, 0); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			t.Skipf("hard links are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if handles, err := openBridgeHandles(paths); err == nil {
		_ = handles.close()
		t.Fatal("hard-linked result swap was accepted")
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "sentinel\n" {
		t.Fatalf("sentinel changed during rejected result swap: %q", contents)
	}
}

func TestExecuteHonorsProtectedCancellationMarker(t *testing.T) {
	if !IsCurrentProcessElevated() {
		t.Skip("native elevated child test requires an administrator token")
	}
	ownerSID, err := currentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := newBridgeFiles(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	request := Request{Schema: SchemaV1, RequestID: "request-cancel", OwnerSID: ownerSID, Operation: OperationOpenSSH, Action: ActionOpenSSHSetup, CancelPath: paths.cancel, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
		t.Fatal(err)
	}
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		time.Sleep(200 * time.Millisecond)
		_ = writeCancellation(paths, ownerSID)
	}()
	err = Execute(context.Background(), paths.request, paths.result, paths.cancel, func(ctx context.Context, _ Request) error {
		<-ctx.Done()
		return ctx.Err()
	})
	<-cancelDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context cancellation", err)
	}
	result, err := readProtectedResult(paths, ownerSID, request.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != statusCanceled || result.ErrorCode != "canceled" {
		t.Fatalf("result = %#v, want canceled result", result)
	}
}

func TestExecutePreservesOperationFailureCode(t *testing.T) {
	if !IsCurrentProcessElevated() {
		t.Skip("native elevated child test requires an administrator token")
	}
	for _, testCase := range []struct {
		name    string
		failure error
		code    string
		message string
	}{
		{"generic", errors.New("deliberate operation failure"), operationFailed, "deliberate operation failure"},
		{"named-install-permission", namedInstallPermissionFailure{}, "install_candidate", "Windows installation failed while preparing the replacement executable."},
		{"maintenance", &RemoteError{Code: "maintenance_busy", Message: "protected work remains"}, "maintenance_busy", "elevated Windows operation failed (maintenance_busy): protected work remains"},
		{"signed-owner-pending", &RemoteError{Code: "activation_pending", Message: "wait for the approved update owner"}, "activation_pending", "elevated Windows operation failed (activation_pending): wait for the approved update owner"},
		{"joined-signed-owner-io", errors.Join(&RemoteError{Code: "activation_pending", Message: "wait for the approved update owner"}, errors.New("independent IO")), operationFailed, "elevated Windows operation failed (activation_pending): wait for the approved update owner independent IO"},
		{"verification", &RemoteError{Code: "update_verification_failed", Message: "invalid proof"}, "update_verification_failed", "elevated Windows operation failed (update_verification_failed): invalid proof"},
		{"live-legacy", &RemoteError{Code: "legacy_carrier_running", Message: "finish active sessions before stopping"}, "legacy_carrier_running", "elevated Windows operation failed (legacy_carrier_running): finish active sessions before stopping"},
		{"joined-legacy-io", errors.Join(&RemoteError{Code: "legacy_carrier_running", Message: "finish active sessions before stopping"}, errors.New("independent IO")), operationFailed, "elevated Windows operation failed (legacy_carrier_running): finish active sessions before stopping independent IO"},
		{"joined-verification-io", errors.Join(&RemoteError{Code: "update_verification_failed", Message: "invalid proof"}, errors.New("independent IO")), operationFailed, "elevated Windows operation failed (update_verification_failed): invalid proof independent IO"},
		{"unknown-code", &RemoteError{Code: "unrecognized", Message: "operation failed"}, operationFailed, "elevated Windows operation failed (unrecognized): operation failed"},
		{"joined-busy-io", errors.Join(&RemoteError{Code: "maintenance_busy", Message: "protected work remains"}, errors.New("independent I/O failure")), operationFailed, "elevated Windows operation failed (maintenance_busy): protected work remains independent I/O failure"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ownerSID, err := currentOwnerSID()
			if err != nil {
				t.Fatal(err)
			}
			paths, cleanup, err := newBridgeFiles(ownerSID)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			request := Request{Schema: SchemaV1, RequestID: "request-failure", OwnerSID: ownerSID, Operation: OperationOpenSSH, Action: ActionOpenSSHSetup, CancelPath: paths.cancel, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
			if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
				t.Fatal(err)
			}
			want := testCase.failure
			err = Execute(context.Background(), paths.request, paths.result, paths.cancel, func(context.Context, Request) error { return want })
			if !errors.Is(err, want) {
				t.Fatalf("Execute error = %v, want operation failure", err)
			}
			result, err := readProtectedResult(paths, ownerSID, request.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != statusError || result.ErrorCode != testCase.code || result.ErrorMessage != testCase.message {
				t.Fatalf("result = %#v, want operation failure", result)
			}
			if testCase.name == "named-install-permission" && (result.ErrorCause != "permission_denied" || result.ErrorErrno != 5) {
				t.Fatalf("native cause lost across protected bridge: %#v", result)
			}
		})
	}
}

type namedInstallPermissionFailure struct{}

func (namedInstallPermissionFailure) Error() string                { return "private-token-and-path" }
func (namedInstallPermissionFailure) Unwrap() error                { return windows.ERROR_ACCESS_DENIED }
func (namedInstallPermissionFailure) ElevationFailureCode() string { return "install_candidate" }

func TestProtectedBridgeRejectsPrivateFaultMetadata(t *testing.T) {
	ownerSID, err := currentOwnerSID()
	if err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := newBridgeFiles(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	request := Request{Schema: SchemaV1, RequestID: "fault", OwnerSID: ownerSID, Operation: OperationRuntimeService, Action: ActionPurge, CancelPath: paths.cancel, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := writeProtectedJSON(paths.request, request, ownerSID); err != nil {
		t.Fatal(err)
	}
	for _, result := range []Result{
		{Schema: SchemaV1, RequestID: "fault", Status: statusError, ErrorCause: "private-token"},
		{Schema: SchemaV1, RequestID: "fault", Status: statusError, ErrorCause: "internal", ErrorErrno: -1},
		{Schema: SchemaV1, RequestID: "fault", Status: statusError, ErrorCause: "internal", ErrorErrno: 65536},
	} {
		if err := writeProtectedJSON(paths.result, result, ownerSID); err != nil {
			t.Fatal(err)
		}
		if _, err := readProtectedResult(paths, ownerSID, "fault"); !errors.Is(err, ErrElevationProtocol) {
			t.Fatalf("unsafe fault metadata accepted: %v", err)
		}
	}
}
