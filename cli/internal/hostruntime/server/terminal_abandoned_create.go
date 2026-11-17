package server

import (
	"context"
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// A create runs under the server's context, not its connection's, so a slow launch finishes after its requester
// gave up: the control plane's create call (helperruntime CreateTerminal) times out and closes its connection, the
// user's close and delete then find nothing on the host, and the late shell runs with no session record (F21-164: 34
// after 100 slow opens on a loaded host). A fresh shell whose create answer cannot be delivered is closed again: the
// requester never learned of it, and a retry of the same create starts it again (create-or-get), so nothing is lost.
// A delivered create stays, attached or not: the control plane owns it from then on.
//
// shortcut: an answer written just as the requester's deadline passes counts as delivered; the server's own sweep
// (terminal_reconcile.go) only checks rows against the host, so that narrow window could still leave a shell.

// freshTerminalCreate reports the session a terminal create request has just started (not an existing one).
func freshTerminalCreate(frame protocol.Frame, outcome operation.Outcome, replay bool) (string, bool) {
	if frame.Capability != "terminal.v1" || replay {
		return "", false
	}
	var request struct {
		Action string `json:"action"`
	}
	var created struct {
		ID       string `json:"id"`
		Existing bool   `json:"existing"`
	}
	if json.Unmarshal(frame.Payload, &request) != nil || request.Action != "create" || json.Unmarshal(outcome.Result, &created) != nil || created.Existing || created.ID == "" {
		return "", false
	}
	return created.ID, true
}

// closeUndeliveredCreate closes a session whose create answer did not reach its requester, unless someone attached
// it meanwhile.
// shortcut: the participant check and the close are two calls; an attach landing between them sees the session closed
// and its client restarts it (RestartIfNotRunning). Upgrade to a close-if-unattached manager call if that is ever seen.
func (s *Server) closeUndeliveredCreate(sessionID string, authorization Authorization) {
	ctx, cancel := context.WithTimeout(s.ctx, s.config.MutationDeadline)
	defer cancel()
	request := func(action string) operation.Outcome {
		payload, _ := json.Marshal(map[string]string{"action": action, "session_id": sessionID})
		return s.config.Handler.Handle(ctx, authorization, "terminal.v1", payload)
	}
	snapshot := request("snapshot")
	var value struct {
		Participants []json.RawMessage `json:"participants"`
	}
	if snapshot.ErrorCode != "" || json.Unmarshal(snapshot.Result, &value) != nil || len(value.Participants) != 0 {
		return
	}
	closed := request("close")
	diagnosticlog.TryInfo("terminal closed", "stage", "undelivered_create", "result", closed.ErrorCode)
}
