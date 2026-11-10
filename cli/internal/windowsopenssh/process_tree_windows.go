//go:build windows

package windowsopenssh

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// containProcessTree puts a started child in a kill-on-close Job Object, so that cancelling the step's context (which
// kills only powershell.exe) also ends the winget/installer processes it spawned once release runs.
func containProcessTree(command *exec.Cmd) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.Close(job)
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		windows.Close(job)
		return nil, err
	}
	defer windows.Close(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		windows.Close(job)
		return nil, err
	}
	return func() { windows.Close(job) }, nil
}
