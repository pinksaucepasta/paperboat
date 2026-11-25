package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
)

// snapshotsBytes leaves room in a control frame; sessions past it are left out and asked for one by one.
const snapshotsBytes = 48 << 10

// snapshotsWait leaves the control plane time to list the rest of its page.
const snapshotsWait = time.Second

type terminalSnapshotResult struct {
	SessionID string          `json:"session_id"`
	Snapshot  json.RawMessage `json:"snapshot,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// terminalSnapshots answers the control plane's list of this machine's sessions in one request. The signed request
// hash names exactly those sessions; without it an owner credential reads only its own session.
func (d *Dispatcher) terminalSnapshots(ctx context.Context, authorization Authorization, ids []string) operation.Outcome {
	if authorization.TerminalRole != TerminalRoleOwner || authorization.BrowserTerminal || len(ids) == 0 || len(ids) > terminalprotocol.MaxSnapshotSessions || ids[0] != authorization.SessionID || authorization.RequestHash != terminalprotocol.SnapshotsHash(ids) {
		return failure("not_found_or_forbidden")
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotsWait)
	defer cancel()
	results := make([]terminalSnapshotResult, len(ids))
	type answer struct {
		index   int
		outcome operation.Outcome
	}
	answers := make(chan answer, len(ids))
	for index, id := range ids { // together: an owner that does not answer loses only its own sessions
		results[index] = terminalSnapshotResult{SessionID: id, Error: "deadline_exceeded"}
		go func() {
			value, err := d.config.Sessions.SnapshotAtGeneration(id, authorization.TerminalGeneration)
			answers <- answer{index, domainResult(value, err)}
		}()
	}
collect:
	for range ids {
		select {
		case found := <-answers:
			results[found.index].Error = found.outcome.ErrorCode
			if found.outcome.ErrorCode == "" {
				results[found.index].Snapshot = found.outcome.Result
			}
		case <-ctx.Done():
			break collect
		}
	}
	size, fit := 0, results[:0]
	for _, value := range results {
		if size += len(value.Snapshot) + 256; size <= snapshotsBytes {
			fit = append(fit, value)
		}
	}
	return result(struct {
		Sessions []terminalSnapshotResult `json:"sessions"`
	}{fit})
}
