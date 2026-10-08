//go:build windows

package service

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEnrolledProcessLaunchIsSilentAndSuspended(t *testing.T) {
	want := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED | windows.EXTENDED_STARTUPINFO_PRESENT)
	if got := enrolledProcessCreationFlags(); got != want {
		t.Fatalf("creation flags=%#x want=%#x", got, want)
	}
	startup := enrolledProcessStartupInfo()
	if startup.Flags&windows.STARTF_USESHOWWINDOW == 0 || startup.ShowWindow != windows.SW_HIDE {
		t.Fatalf("startup does not hide the enrolled process: %+v", startup)
	}
}

func TestEnrolledProcessOwnsUsablePrivateStandardHandles(t *testing.T) {
	startup, closeStartup, err := enrolledProcessStartupWithStdio()
	if err != nil {
		t.Fatal(err)
	}
	defer closeStartup()
	if startup.Flags&windows.STARTF_USESTDHANDLES == 0 || startup.ProcThreadAttributeList == nil {
		t.Fatal("standard handles lack an explicit inheritance list")
	}
	var written uint32
	for _, handle := range []windows.Handle{startup.StdOutput, startup.StdErr} {
		if err := windows.WriteFile(handle, []byte("private-probe"), &written, nil); err != nil {
			t.Fatal(err)
		}
	}
	handles := []windows.Handle{startup.StdInput, startup.StdOutput, startup.StdErr}
	closeStartup()
	for _, handle := range handles {
		if _, err := windows.GetFileType(handle); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatal("owned standard handle remained open")
		}
	}
}
