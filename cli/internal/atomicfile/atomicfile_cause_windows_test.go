//go:build windows

package atomicfile

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type atomicPrivateError struct{}

func (*atomicPrivateError) Error() string { panic("private error formatted") }

func TestWindowsAtomicReplacementFailureRetainsCauseAndRecovers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private-destination.json")
	options := Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}
	if err := Write(path, []byte("original"), options); err != nil {
		t.Fatal("initial protected write failed")
	}
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(nativePath, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal("fixture could not hold destination")
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(handle)
		}
	}()
	err = Write(path, []byte("replacement"), options)
	var failure *Error
	if !errors.As(err, &failure) || failure.Stage != StageReplace || !(errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)) {
		var errno syscall.Errno
		errors.As(err, &errno)
		stage := Stage("")
		if failure != nil {
			stage = failure.Stage
		}
		t.Fatalf("replacement failure stage=%s errno=%d", stage, errno)
	}
	var nativeCode syscall.Errno
	errors.As(err, &nativeCode)
	t.Logf("owned replacement refusal stage=%s errno=%d", failure.Stage, nativeCode)
	if err.Error() != "atomic file write failed" {
		t.Fatal("atomic error exposed path or cause")
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "original" {
		t.Fatal("failed replacement changed destination")
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatal("failed replacement left staging")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := Write(path, []byte("replacement"), options); err != nil {
		t.Fatal("fresh replacement did not recover")
	}
	contents, readErr = os.ReadFile(path)
	if readErr != nil || string(contents) != "replacement" {
		t.Fatal("recovery destination incorrect")
	}
}

func TestWindowsAtomicNativeHandleProbeFailureAndRecovery(t *testing.T) {
	root := t.TempDir()
	descriptor, err := currentOwnerSecurityDescriptor()
	if err != nil {
		t.Fatal("resolve owner failed")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("owner lookup failed")
	}
	descriptor = "O:" + user.User.Sid.String() + descriptor
	file, path, err := createProtectedTemporary(root, descriptor)
	if err != nil {
		t.Fatal("protected temporary creation failed")
	}
	defer os.Remove(path)
	defer file.Close()
	handle := windows.Handle(file.Fd())
	if matches, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, user.User.Sid); err != nil || !matches {
		t.Fatal("valid native owner probe failed")
	}
	if matches, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor); err != nil || !matches {
		t.Fatal("valid native DACL probe failed")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if matches, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, user.User.Sid); matches || !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatal("closed owner probe lost native cause")
	}
	if matches, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor); matches || !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatal("closed DACL probe lost native cause")
	}
	if windowssecurity.HandleOwnerMatchesSID(handle, user.User.Sid) || windowssecurity.ProtectedHandleDACLMatches(handle, descriptor) {
		t.Fatal("boolean probes did not fail closed")
	}
	if err := Write(filepath.Join(root, "fresh.json"), []byte("fresh"), Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		t.Fatal("fresh protected write did not recover")
	}
	spy := &atomicPrivateError{}
	failure := &Error{Stage: StageOwner, Path: "private", Err: spy}
	if failure.Error() != "atomic file write failed" || !errors.Is(failure, spy) {
		t.Fatal("static atomic owner lost original cause")
	}
	var empty *Error
	if empty.Unwrap() != nil || empty.Error() != "atomic file write failed" {
		t.Fatal("nil atomic error unsafe")
	}
}
