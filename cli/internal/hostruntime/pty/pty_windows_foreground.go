//go:build windows

package pty

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	systemExtendedHandleInformation = 64
	statusInfoLengthMismatch        = 0xC0000004
)

// extendedHandle is SYSTEM_HANDLE_TABLE_ENTRY_INFO_EX.
type extendedHandle struct {
	object     uintptr
	pid        uintptr
	handle     uintptr
	access     uint32
	backTrace  uint16
	typeIndex  uint16
	attributes uint32
	reserved   uint32
}

// shellIsCommandInterpreter reports whether the session's shell is cmd.exe, the default (%ComSpec%). Only there does the process
// handle rule below identify a foreground program; for any other shell (PowerShell keeps handles to its job workers and background
// processes while idle) nothing may be terminated on a Ctrl-C: missing a cancel is better than killing background work.
func (p *Process) shellIsCommandInterpreter() bool {
	return strings.EqualFold(imageBase(p.processID), "cmd.exe")
}

// imageBase is the file name of a process's executable, or "" when it cannot be read.
func imageBase(pid uint32) string {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size) != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buffer[:size]))
}

// processesHeldBy returns the pids of the processes the process shell holds an open handle to, or nil when the handle table cannot be
// read. cmd.exe waiting for a foreground program holds a process handle to it (for the wait and %ERRORLEVEL%); at an idle prompt and
// for `start /b` children it holds none (measured on Windows 11, 5 of 5 states).
func processesHeldBy(shell uint32) map[uint32]bool {
	own, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, windows.GetCurrentProcessId())
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(own)
	entries := handleTable()
	if entries == nil {
		return nil
	}
	typeIndex, found := uint16(0), false
	for _, entry := range entries {
		if entry.pid == uintptr(windows.GetCurrentProcessId()) && entry.handle == uintptr(own) {
			typeIndex, found = entry.typeIndex, true
			break
		}
	}
	if !found {
		return nil
	}
	source, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, shell)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(source)
	held := map[uint32]bool{}
	for _, entry := range entries {
		if entry.pid != uintptr(shell) || entry.typeIndex != typeIndex {
			continue
		}
		var duplicate windows.Handle
		if windows.DuplicateHandle(source, windows.Handle(entry.handle), windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
			continue
		}
		if pid, err := windows.GetProcessId(duplicate); err == nil && pid != 0 {
			held[pid] = true
		}
		windows.CloseHandle(duplicate)
	}
	return held
}

// handleTable reads SystemExtendedHandleInformation, growing the buffer until it fits.
func handleTable() []extendedHandle {
	size := uint32(1 << 20)
	for attempt := 0; attempt < 8; attempt++ {
		buffer := make([]byte, size)
		var needed uint32
		err := windows.NtQuerySystemInformation(systemExtendedHandleInformation, unsafe.Pointer(&buffer[0]), size, &needed)
		if err == nil {
			count := *(*uintptr)(unsafe.Pointer(&buffer[0]))
			header := 2 * unsafe.Sizeof(uintptr(0))
			stride := unsafe.Sizeof(extendedHandle{})
			if uintptr(len(buffer)) < header+count*stride {
				return nil
			}
			return append([]extendedHandle(nil), unsafe.Slice((*extendedHandle)(unsafe.Pointer(&buffer[header])), count)...)
		}
		if status, ok := err.(windows.NTStatus); !ok || uint32(status) != statusInfoLengthMismatch {
			return nil
		}
		size = needed + needed/4 + 1<<16
	}
	return nil
}
