//go:build linux

package machineservices

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Listener caps: the fd walk stops early once every LISTEN inode resolves.
const (
	maxListenerProcsScanned = 4096
	maxListenerFDsPerProc   = 1024
)

// procRoot is /proc; a var so tests point it at a fake tree.
var procRoot = "/proc"

// listenerOwner finds LISTEN inodes for port in both tcp tables (bounded by
// maximumSnapshotBytes, as in snapshot in discovery_linux.go), resolves them
// to pids through /proc/<pid>/fd, and names the smallest pid via
// /proc/<pid>/comm. An fd directory that cannot be listed (another user's
// process) skips that pid; an inode with no resolvable pid means the pid is
// genuinely unknown, so ok is false.
func listenerOwner(port uint16) (string, int, bool) {
	remaining := int64(maximumSnapshotBytes)
	inodes := make(map[uint64]bool)
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := readProcNetFile(path, remaining)
		if err != nil {
			continue
		}
		remaining -= int64(len(data))
		for _, inode := range listenInodesProcTCP(data, strings.HasSuffix(path, "tcp6"), port) {
			inodes[inode] = true
		}
	}
	if len(inodes) == 0 {
		return "", 0, false
	}
	pids := pidsForSocketInodes(procRoot, inodes)
	if len(pids) == 0 {
		return "", 0, false
	}
	pid := pids[0]
	return commOf(procRoot, pid), pid, true
}

func readProcNetFile(path string, remaining int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := readBounded(file, remaining)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

// pidsForSocketInodes returns the sorted pids holding one of want, read from
// root/<pid>/fd symlinks of the form "socket:[inode]". Unreadable fd
// directories (other users, exited processes) are skipped, which is exactly
// the "pid without a name / unknown pid" case of the brief.
func pidsForSocketInodes(root string, want map[uint64]bool) []int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var pids []int
	scanned := 0
	for _, entry := range entries {
		pid64, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil || pid64 == 0 || !entry.IsDir() {
			continue
		}
		if scanned >= maxListenerProcsScanned {
			break
		}
		scanned++
		fds, err := os.ReadDir(filepath.Join(root, entry.Name(), "fd"))
		if err != nil {
			continue
		}
		links := 0
		for _, fd := range fds {
			if links >= maxListenerFDsPerProc {
				break
			}
			target, err := os.Readlink(filepath.Join(root, entry.Name(), "fd", fd.Name()))
			links++
			if err != nil {
				continue
			}
			inode, ok := parseSocketLink(target)
			if !ok || !want[inode] {
				continue
			}
			pids = append(pids, int(pid64))
			break
		}
		if len(pids) >= maxListenerOwners {
			break
		}
	}
	sort.Ints(pids)
	return pids
}

func parseSocketLink(target string) (uint64, bool) {
	if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	inode, err := strconv.ParseUint(target[len("socket:["):len(target)-1], 10, 64)
	if err != nil || inode == 0 {
		return 0, false
	}
	return inode, true
}

// commOf returns the first line of root/<pid>/comm: the executable base name
// the kernel maintains, so no path parsing is needed. Unreadable (another
// user) or empty means unknown, not an error.
func commOf(root string, pid int) string {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(name)
}
