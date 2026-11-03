package session

import (
	"context"
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
)

func (f *Fanout) attachmentLease(id string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor := f.attachments[id]; cursor != nil {
		return cursor.lease
	}
	return 0
}

// DetachAttachment releases only this transport's attachment. A late old
// output-stream cleanup is a no-op after another carrier takes it over.
func (m *Manager) DetachAttachment(sessionID, attachmentID string, lease uint64) error {
	if lease == 0 {
		return ErrInvalidInput
	}
	s, err := m.get(sessionID)
	if err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.fanout.attachmentLease(attachmentID) != lease {
		return nil
	}
	if err := s.fanout.Detach(attachmentID); err != nil {
		return err
	}
	s.removeParticipantLocked(attachmentID)
	m.applyWindowSizeObservedLocked(s)
	return nil
}

// WaitNextAttachment fences the queue lookup and advancement together. An old
// reader cannot consume the replacement's replay after an attachment takeover.
func (m *Manager) WaitNextAttachment(ctx context.Context, sessionID, attachmentID string, from, lease uint64) (history.Event, error) {
	if lease == 0 {
		return history.Event{}, ErrInvalidInput
	}
	s, err := m.get(sessionID)
	if err != nil {
		return history.Event{}, err
	}
	return s.fanout.waitNextFromLease(ctx, attachmentID, from, lease)
}

type attachmentLeaseRequest struct {
	Sessionid    string
	Attachmentid string
	From         uint64
	Lease        uint64
}

func (m *RemoteManager) DetachAttachment(sessionID, attachmentID string, lease uint64) error {
	var out struct{}
	return m.invoke(context.Background(), "DetachAttachment", attachmentLeaseRequest{Sessionid: sessionID, Attachmentid: attachmentID, Lease: lease}, &out)
}
func (m *RemoteManager) WaitNextAttachment(ctx context.Context, sessionID, attachmentID string, from, lease uint64) (history.Event, error) {
	var out struct{ V0 history.Event }
	err := m.invoke(ctx, "WaitNextAttachment", attachmentLeaseRequest{Sessionid: sessionID, Attachmentid: attachmentID, From: from, Lease: lease}, &out)
	return out.V0, err
}

func serveAttachmentLease(ctx context.Context, m Service, op string, body json.RawMessage) (json.RawMessage, error) {
	var args attachmentLeaseRequest
	if err := decodeRemoteRequest(body, &args); err != nil {
		return nil, ErrInvalidSession
	}
	if op == "DetachAttachment" {
		if err := m.DetachAttachment(args.Sessionid, args.Attachmentid, args.Lease); err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	}
	event, err := m.WaitNextAttachment(ctx, args.Sessionid, args.Attachmentid, args.From, args.Lease)
	if err != nil {
		return nil, err
	}
	defer event.Release()
	return json.Marshal(struct{ V0 history.Event }{event})
}
