package windowsopenssh

import (
	"slices"
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
