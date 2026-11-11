package localdaemon

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// attachedTerminals are the terminal links this daemon carries for pb connect, newest last, so that
// pb recordings start|stop can send the control on the user's own attachment.
var attachedTerminals = &terminalLinks{}

type terminalLinks struct {
	mu    sync.Mutex
	links []tunnel.RecordingController
}

func (t *terminalLinks) add(link tunnel.RecordingController) (remove func()) {
	t.mu.Lock()
	t.links = append(t.links, link)
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.links = slices.DeleteFunc(t.links, func(l tunnel.RecordingController) bool { return l == link })
		t.mu.Unlock()
	}
}

// control picks the attached session (the only one when none is named) and sends the action on its
// newest link: a reconnect leaves the old link closing until its stream ends.
func (t *terminalLinks) control(ctx context.Context, request localapi.RecordingControlRequest) (localapi.RecordingControlResult, error) {
	t.mu.Lock()
	var sessions []string
	var chosen tunnel.RecordingController
	for _, link := range t.links {
		id := link.RecordingSessionID()
		if id == "" || request.SessionID != "" && id != request.SessionID {
			continue
		}
		if !slices.Contains(sessions, id) {
			sessions = append(sessions, id)
		}
		chosen = link
	}
	t.mu.Unlock()
	switch {
	case len(sessions) == 0:
		return localapi.RecordingControlResult{}, &localapi.RecordingControlError{Code: localapi.RecordingNotAttachedCode, Message: "no pb connect on this computer is attached to that session"}
	case len(sessions) > 1:
		return localapi.RecordingControlResult{}, &localapi.RecordingControlError{Code: localapi.RecordingAmbiguousCode, Message: "several sessions are attached; name one: " + strings.Join(sessions, ", ")}
	}
	state, err := chosen.RecordingControl(ctx, request.Action)
	if err != nil {
		return localapi.RecordingControlResult{}, err
	}
	return localapi.RecordingControlResult{SessionID: sessions[0], Recording: state}, nil
}

// RecordingControl is the local API's recording start/stop.
func RecordingControl(ctx context.Context, _ localapi.Peer, request localapi.RecordingControlRequest) (localapi.RecordingControlResult, error) {
	return attachedTerminals.control(ctx, request)
}
