package localdaemon

import (
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
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
	// resume is the last target that held a resume secret, while target itself has none: a redial that never reached
	// the host (the network was down) must not cost the terminal its way back without Paperboat.
	resume *resolver.TerminalTarget
	seen   time.Time
	ended  time.Time // zero while the owner's stream is open
}

const (
	// An entry outlives its stream by inputOwnerTTL, so a redial after a clean close finds it; an open stream's entry
	// stays for inputOwnerLiveMax, so a terminal held for hours can still resume.
	inputOwnerTTL     = 10 * time.Minute
	inputOwnerLiveMax = 7 * 24 * time.Hour
	inputOwnerMax     = 256
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
	o.expireLocked(now)
	var resume *resolver.TerminalTarget
	if previous, ok := o.entries[key]; ok {
		resume = previous.resume
		if len(previous.target.ResumeSecret) != 0 {
			resume = previous.target
		}
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
	o.entries[key] = inputOwnerEntry{target: target, resume: resume, seen: now}
}

func (o *inputOwners) expireLocked(now time.Time) {
	for k, e := range o.entries {
		if !e.ended.IsZero() && now.Sub(e.ended) > inputOwnerTTL || e.ended.IsZero() && now.Sub(e.seen) > inputOwnerLiveMax {
			delete(o.entries, k)
		}
	}
}

// finish records that target's stream ended; its entry then expires inputOwnerTTL later.
func (o *inputOwners) finish(owner, machineID string, target *resolver.TerminalTarget, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := inputOwnerKey{owner, machineID, target.SessionID}
	if e, ok := o.entries[key]; ok && e.target == target {
		e.ended = now
		o.entries[key] = e
	}
}

// takeResume hands the owner's resume secret and the access session and attachment it belongs to to a redial, once:
// the secret is single use, and the redial's own attach brings the rotated one.
func (o *inputOwners) takeResume(owner, machineID, sessionID string, now time.Time) (secret []byte, resource, attachmentID string, ok bool) {
	if owner == "" || sessionID == "" {
		return nil, "", "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked(now)
	e, found := o.entries[inputOwnerKey{owner, machineID, sessionID}]
	if !found {
		return nil, "", "", false
	}
	held := e.target
	if len(held.ResumeSecret) == 0 {
		held = e.resume
	}
	if held == nil || len(held.ResumeSecret) == 0 || held.Auth.ResourceID == "" || held.InputAttachmentID == "" {
		return nil, "", "", false
	}
	secret, held.ResumeSecret = held.ResumeSecret, nil
	return secret, held.Auth.ResourceID, held.InputAttachmentID, true
}
