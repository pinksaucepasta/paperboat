package localdaemon

import (
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// inputOwners lets one pb connect keep its terminal input across a full redial: the next peer stream from the same
// input owner, machine and session reuses the previous target's input queue and attachment, so the host resends only
// what it has not answered and its journal drops repeats. Bounded: entries expire and the map is capped.
type inputOwners struct {
	mu      sync.Mutex
	entries map[inputOwnerKey]inputOwnerEntry
}

type inputOwnerKey struct{ owner, machineID, sessionID string }

type inputOwnerEntry struct {
	target *resolver.TerminalTarget
	seen   time.Time
}

const (
	inputOwnerTTL = 10 * time.Minute
	inputOwnerMax = 256
)

var terminalInputOwners = &inputOwners{entries: make(map[inputOwnerKey]inputOwnerEntry)}

// adopt gives target the input state of the owner's previous target, if any, and records target as the latest.
func (o *inputOwners) adopt(owner, machineID string, target *resolver.TerminalTarget, now time.Time) {
	if owner == "" || target.SessionID == "" {
		return
	}
	key := inputOwnerKey{owner, machineID, target.SessionID}
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, e := range o.entries {
		if now.Sub(e.seen) > inputOwnerTTL {
			delete(o.entries, k)
		}
	}
	if previous, ok := o.entries[key]; ok {
		if target.InputQueue == nil {
			target.InputQueue = previous.target.InputQueue
		}
		if target.InputAttachmentID == "" {
			target.InputAttachmentID = previous.target.InputAttachmentID
		}
	}
	if target.InputQueue == nil {
		target.InputQueue = resolver.NewTerminalInputQueue(256)
	}
	if _, ok := o.entries[key]; !ok && len(o.entries) >= inputOwnerMax {
		return
	}
	o.entries[key] = inputOwnerEntry{target: target, seen: now}
}
