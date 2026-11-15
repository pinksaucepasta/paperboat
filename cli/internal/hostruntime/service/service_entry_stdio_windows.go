//go:build windows

package service

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// serviceLogLimit bounds a service log: a larger one starts over when the service starts.
const serviceLogLimit = 1 << 20

// serviceLogSecurity is SYSTEM and Administrators only, like the startup-error logs beside it.
const serviceLogSecurity = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

// openServiceLog opens (creating it protected) the append-only, inheritable handle a workload writes its output to.
func openServiceLog(path string) (windows.Handle, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > serviceLogLimit {
		if err := os.Truncate(path, 0); err != nil {
			return 0, err
		}
	}
	descriptor, err := windows.SecurityDescriptorFromString(serviceLogSecurity)
	if err != nil {
		return 0, err
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor, InheritHandle: 1}
	return windows.CreateFile(windows.StringToUTF16Ptr(path), windows.FILE_APPEND_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &security, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
}

// A service has no console. Give its owner workload valid, private streams and inherit exactly those handles, never
// the privileged parent's other handles. Output goes to logPath (the Unix units' StandardOutPath/StandardErrorPath),
// or to NUL when the service names no log.
func enrolledProcessStartupWithStdio(logPath string) (*windows.StartupInfoEx, func(), error) {
	handles := make([]windows.Handle, 0, 3)
	var attributes *windows.ProcThreadAttributeListContainer
	closeStartup := func() {
		if attributes != nil {
			attributes.Delete()
			attributes = nil
		}
		for _, handle := range handles {
			_ = windows.CloseHandle(handle)
		}
		handles = nil
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	for i, access := range []uint32{windows.GENERIC_READ, windows.GENERIC_WRITE, windows.GENERIC_WRITE} {
		var handle windows.Handle
		var err error
		if i > 0 && logPath != "" {
			handle, err = openServiceLog(logPath)
		} else {
			handle, err = windows.CreateFile(windows.StringToUTF16Ptr("NUL"), access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &security, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		}
		if err != nil {
			closeStartup()
			return nil, nil, err
		}
		handles = append(handles, handle)
	}
	var err error
	attributes, err = windows.NewProcThreadAttributeList(1)
	if err != nil {
		closeStartup()
		return nil, nil, err
	}
	if err = attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		closeStartup()
		return nil, nil, err
	}
	startup := &windows.StartupInfoEx{StartupInfo: enrolledProcessStartupInfo()}
	startup.Cb = uint32(unsafe.Sizeof(*startup))
	startup.Flags |= windows.STARTF_USESTDHANDLES
	startup.StdInput, startup.StdOutput, startup.StdErr = handles[0], handles[1], handles[2]
	startup.ProcThreadAttributeList = attributes.List()
	return startup, closeStartup, nil
}
