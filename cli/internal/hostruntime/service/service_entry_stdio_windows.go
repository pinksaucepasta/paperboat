//go:build windows

package service

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

// A service has no console. Give its owner workload valid, private NUL streams
// and inherit exactly those handles, never the privileged parent's other handles.
func enrolledProcessStartupWithStdio() (*windows.StartupInfoEx, func(), error) {
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
	for _, access := range []uint32{windows.GENERIC_READ, windows.GENERIC_WRITE, windows.GENERIC_WRITE} {
		handle, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &security, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
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
