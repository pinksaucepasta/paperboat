//go:build windows

package windowsopenssh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// TerminateManagedSessions ends the open sessions of this instance's PaperboatSshd generations. Win32-OpenSSH checks
// authorized keys at login only and keeps a session alive after its connection closes, so a revoked key needs this
// (F5-10). Only the sessions of the removed keys end, found through sshd's VERBOSE log (Accepted publickey
// ... SHA256 plus "User child is on pid"); when the log cannot be read, or a key cannot be named, all of them end.
func TerminateManagedSessions(instance, stateRoot string, removed []string) error {
	services, logs, failed := managedServiceHostPIDs(instance, stateRoot) // a service that could not be read is reported, the rest still end
	if len(services) == 0 {
		return failed
	}
	// Only the removed keys' sessions end when the log names them; any doubt ends them all.
	endKeys, pidKey, conflicts := map[string]bool{}, map[uint32]string{}, map[uint32]bool{}
	for _, key := range removed {
		endKeys[key] = true
	}
	if len(removed) == 0 || endKeys[AllSessions] {
		endKeys = nil
	}
	for _, path := range logs {
		if err := readSessionLog(path, pidKey, conflicts); err != nil {
			endKeys = nil
		}
	}
	for pid := range conflicts { // two logs name one live pid differently: unknown, so it ends
		delete(pidKey, pid)
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
	for _, pid := range sessionPIDsForKeys(table, services, pidKey, endKeys) {
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
	selected := sessionPIDsForKeys(table, services, pidKey, endKeys)
	for index := len(selected) - 1; index >= 0; index-- { // children first: a session's parent outlives its walk
		if handle, ok := handles[selected[index]]; ok {
			if err := windows.TerminateProcess(handle, 1); err != nil {
				failed = errors.Join(failed, fmt.Errorf("end SSH session process %d: %w", selected[index], err))
			}
		}
	}
	return failed
}

// readSessionLog reads the end of one generation's service log into pidKey. shortcut: only the last 8 MiB, older
// sessions are unnamed and so end with any revoke; upgrade with log rotation.
func readSessionLog(path string, pidKey map[uint32]string, conflicts map[uint32]bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	const tail = 8 << 20
	if info.Size() > tail {
		if _, err := file.Seek(info.Size()-tail, io.SeekStart); err != nil {
			return err
		}
	}
	for pid, key := range parseSessionLog(file) {
		if old, ok := pidKey[pid]; ok && old != key {
			conflicts[pid] = true
		}
		pidKey[pid] = key
	}
	return nil
}

func managedServiceHostPIDs(instance, stateRoot string) (map[uint32]bool, []string, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("open service manager to end revoked SSH sessions: %w", err)
	}
	defer manager.Disconnect()
	names, err := manager.ListServices()
	if err != nil {
		return nil, nil, fmt.Errorf("list services to end revoked SSH sessions: %w", err)
	}
	pids := map[uint32]bool{}
	var logs []string
	var failed error
	for _, name := range names {
		generation, ok := managedGeneration(name, instance)
		if !ok {
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
			logs = append(logs, filepath.Join(stateRoot, "generations", generation, "logs", "service.log"))
		}
	}
	return pids, logs, failed
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
