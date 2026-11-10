package windowsopenssh

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type processEntry struct {
	PID, PPID uint32
	Image     string
}

// sessionPIDs selects the open-session processes of the managed sshd: everything below an sshd.exe whose parent is one
// of servicePIDs (the PaperboatSshd service hosts), never the listener itself and never a process outside those trees,
// so another account's stock sshd and its sessions are never selected.
func sessionPIDs(table []processEntry, servicePIDs map[uint32]bool) []uint32 {
	children := map[uint32][]uint32{}
	for _, entry := range table {
		if entry.PID != entry.PPID {
			children[entry.PPID] = append(children[entry.PPID], entry.PID)
		}
	}
	var queue, result []uint32
	for _, entry := range table {
		if servicePIDs[entry.PPID] && !servicePIDs[entry.PID] && strings.EqualFold(entry.Image, "sshd.exe") {
			queue = append(queue, entry.PID)
		}
	}
	listeners := slices.Clone(queue)
	seen := map[uint32]bool{}
	for _, pid := range queue {
		seen[pid] = true
	}
	for len(queue) > 0 { // visited set: a reused PID can close a cycle
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if !seen[child] && !servicePIDs[child] {
				seen[child] = true
				queue = append(queue, child)
				result = append(result, child)
			}
		}
	}
	return slices.DeleteFunc(result, func(pid uint32) bool { return slices.Contains(listeners, pid) })
}

var acceptedKey = regexp.MustCompile(`^Accepted publickey for .* ssh2: [A-Z0-9-]+ (SHA256:[A-Za-z0-9+/]{43})$`)

// parseSessionLog maps the pid sshd names in "User child is on pid N" to the key of the "Accepted publickey ...
// SHA256:<fp>" line before it. The two lines are only paired when exactly one Accepted line is waiting: with two in
// flight (interleaved logins) the order is unknown, so neither pid is mapped and sessionPIDsForKeys ends both. A pid
// logged again takes the newer key (the older process is dead).
func parseSessionLog(log io.Reader) (map[uint32]string, error) {
	pidKey := map[uint32]string{}
	var pending []string
	scanner := bufio.NewScanner(log)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if match := acceptedKey.FindStringSubmatch(line); match != nil {
			pending = append(pending, match[1])
		} else if rest, ok := strings.CutPrefix(line, "User child is on pid "); ok {
			if pid, err := strconv.ParseUint(rest, 10, 32); err == nil && len(pending) == 1 {
				pidKey[uint32(pid)] = pending[0]
			} else if err == nil {
				delete(pidKey, uint32(pid))
			}
			pending = nil
		}
	}
	return pidKey, scanner.Err() // an over-long line stops the scan: the map is partial, so the caller must not trust it
}

// sessionPIDsForKeys narrows sessionPIDs to the sessions of the removed keys. Each logged pid names a session's
// process; its ancestors below the listener and everything under it are that session. A live session the log does not
// name (log rotated, lines interleaved) is ended too: over-ending is the safe direction. removed == nil ends all.
func sessionPIDsForKeys(table []processEntry, servicePIDs map[uint32]bool, pidKey map[uint32]string, removed map[string]bool) []uint32 {
	all := sessionPIDs(table, servicePIDs)
	if removed == nil {
		return all
	}
	inAll := map[uint32]bool{}
	for _, pid := range all {
		inAll[pid] = true
	}
	parent, children := map[uint32]uint32{}, map[uint32][]uint32{}
	for _, entry := range table {
		if inAll[entry.PID] {
			parent[entry.PID] = entry.PPID
			children[entry.PPID] = append(children[entry.PPID], entry.PID)
		}
	}
	session := func(root uint32) map[uint32]bool {
		group := map[uint32]bool{}
		for pid := root; inAll[pid] && !group[pid]; pid = parent[pid] {
			group[pid] = true
		}
		for queue := []uint32{root}; len(queue) > 0; queue = queue[1:] {
			for _, child := range children[queue[0]] {
				if !group[child] {
					group[child] = true
					queue = append(queue, child)
				}
			}
		}
		return group
	}
	covered, end := map[uint32]bool{}, map[uint32]bool{}
	for pid, key := range pidKey {
		if !inAll[pid] {
			continue
		}
		for member := range session(pid) {
			covered[member] = true
			if removed[key] {
				end[member] = true
			}
		}
	}
	var result []uint32
	for _, pid := range all {
		if end[pid] || !covered[pid] {
			result = append(result, pid)
		}
	}
	return result
}

// managedGeneration returns the generation of a PaperboatSshd-<instance>-<generation> service name, matching the
// whole name (a 32-digit lowercase hex generation), so instance "u-1" never matches "u-10".
func managedGeneration(name, instance string) (string, bool) {
	generation, ok := strings.CutPrefix(name, ServiceName+"-"+instance+"-")
	if _, err := hex.DecodeString(generation); !ok || err != nil || len(generation) != 32 || strings.ToLower(generation) != generation {
		return "", false
	}
	return generation, true
}

// TerminateResult says what a revoke ended. Fallback is empty for a targeted revoke; otherwise it names why every managed
// session was ended instead of only the removed keys'.
type TerminateResult struct {
	Ended    int
	Fallback string
}

// sessionKeysFromLogs reads the pid-to-key map from the generations' service logs and the set of removed keys to end. A
// missing or unreadable log, an unparsable one, no removed key or a key that could not be named gives endKeys == nil
// (end everything) and the reason in fallback.
func sessionKeysFromLogs(paths, removed []string) (endKeys map[string]bool, pidKey map[uint32]string, fallback string) {
	endKeys, pidKey = map[string]bool{}, map[uint32]string{}
	for _, key := range removed {
		endKeys[key] = true
	}
	switch {
	case len(removed) == 0:
		fallback = "no removed key was named"
	case endKeys[AllSessions]:
		fallback = "a removed key could not be fingerprinted"
	}
	conflicts := map[uint32]bool{}
	for _, path := range paths {
		keys, err := readSessionLog(path)
		if err != nil && fallback == "" {
			fallback = fmt.Sprintf("the SSH service log could not be read: %v", err)
		}
		for pid, key := range keys {
			if old, ok := pidKey[pid]; ok && old != key {
				conflicts[pid] = true // two logs name one live pid differently: unknown, so it ends
			}
			pidKey[pid] = key
		}
	}
	for pid := range conflicts {
		delete(pidKey, pid)
	}
	if fallback != "" {
		endKeys = nil
	}
	return endKeys, pidKey, fallback
}

func readSessionLog(path string) (map[uint32]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseSessionLog(io.LimitReader(file, serviceLogCap))
}
