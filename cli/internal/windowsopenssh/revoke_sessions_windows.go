//go:build windows

package windowsopenssh

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// TerminateManagedSessions ends the open sessions of this instance's PaperboatSshd generations. Win32-OpenSSH checks
// authorized keys at login only and keeps a session alive after its connection closes, so a revoked key needs this
// (F5-10). shortcut: every managed session ends, not only the revoked key's; upgrade by matching sshd's VERBOSE
// "Accepted publickey ... SHA256:" log to the session.
func TerminateManagedSessions(instance string) error {
	services, failed := managedServiceHostPIDs(instance) // a service that could not be read is reported, the rest still end
	if len(services) == 0 {
		return failed
	}
	table, err := processTable()
	if err != nil {
		return err
	}
	// Open every selected process first: an open handle pins its identity, so the table read again afterwards
	// (a reused PID has another parent) decides what is still a session before anything is killed.
	handles := map[uint32]windows.Handle{}
	defer func() {
		for _, handle := range handles {
			windows.CloseHandle(handle)
		}
	}()
	for _, pid := range sessionPIDs(table, services) {
		handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) { // already gone
			continue
		}
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("open SSH session process %d: %w", pid, err))
			continue
		}
		handles[pid] = handle
	}
	if table, err = processTable(); err != nil {
		return errors.Join(failed, err)
	}
	for _, pid := range sessionPIDs(table, services) {
		if handle, ok := handles[pid]; ok {
			if err := windows.TerminateProcess(handle, 1); err != nil {
				failed = errors.Join(failed, fmt.Errorf("end SSH session process %d: %w", pid, err))
			}
		}
	}
	return failed
}

func managedServiceHostPIDs(instance string) (map[uint32]bool, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("open service manager to end revoked SSH sessions: %w", err)
	}
	defer manager.Disconnect()
	names, err := manager.ListServices()
	if err != nil {
		return nil, fmt.Errorf("list services to end revoked SSH sessions: %w", err)
	}
	prefix := ServiceName + "-" + instance + "-"
	pids := map[uint32]bool{}
	var failed error
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		item, err := manager.OpenService(name)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("open service %s: %w", name, err))
			continue
		}
		status, err := item.Query()
		item.Close()
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("query service %s: %w", name, err))
		} else if status.ProcessId != 0 {
			pids[status.ProcessId] = true
		}
	}
	return pids, failed
}

func processTable() ([]processEntry, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot to end revoked SSH sessions: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, fmt.Errorf("process snapshot entry: %w", err)
	}
	var table []processEntry
	for count := 0; count < 1<<20; count++ {
		table = append(table, processEntry{entry.ProcessID, entry.ParentProcessID, syscall.UTF16ToString(entry.ExeFile[:])})
		err := windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return table, nil
		}
		if err != nil {
			return nil, fmt.Errorf("process snapshot entry: %w", err)
		}
	}
	return nil, errors.New("process snapshot exceeds bound")
}
