package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
)

// ResumeSecretSize is the length of an attachment's resume secret (256 bits).
const ResumeSecretSize = 32

// maxResumeFailures bounds wrong resume attempts per issued secret; after that the attachment can
// only reconnect with a fresh server credential.
const maxResumeFailures = 3

// attachmentResume is one attachment's current resume secret. It lives only in the PTY owner's
// memory and ends with the attachment (removeParticipantLocked) or its first successful use.
type attachmentResume struct {
	secret   [ResumeSecretSize]byte
	resource string
	failures int
}

// ResumeRequest moves an attachment to a new connection without a fresh credential. The identity
// and authority fields come from the native session that carries the request, never from the client.
type ResumeRequest struct {
	AttachmentID string
	AccountID    string
	ClientID     string
	// Resource is the access session the peer's current scope covers; Standing says a standing
	// host rule admitted the peer instead.
	Resource     string
	Standing     bool
	Secret       []byte
	FromSequence uint64
}

// IssueResume gives the owner attachment held under lease a fresh resume secret bound to its
// participant and to resource, replacing (rotating) any earlier one.
func (m *Manager) IssueResume(sessionID, attachmentID string, lease uint64, resource string) ([]byte, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return nil, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	participant, ok := session.participants[attachmentID]
	if !ok || lease == 0 || resource == "" || session.fanout.attachmentLease(attachmentID) != lease || participant.Role != "owner" || participant.Browser || participant.AccountID == "" || participant.ClientID == "" {
		return nil, ErrInvalidInput
	}
	record := &attachmentResume{resource: resource}
	if _, err := rand.Read(record.secret[:]); err != nil {
		return nil, err
	}
	if session.resumes == nil {
		session.resumes = make(map[string]*attachmentResume)
	}
	session.resumes[attachmentID] = record
	return bytes.Clone(record.secret[:]), nil
}

// ResumeAttachment takes an attachment over for the same account and client that prove its current
// resume secret, compared in constant time like EternalTerminal's passkey
// (~/ref/EternalTerminal/src/base/ServerClientConnection.cpp:93). A match is single use, even when
// the attach then fails; a mismatch counts against maxResumeFailures.
func (m *Manager) ResumeAttachment(sessionID string, request ResumeRequest) (AttachResult, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return AttachResult{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	record, issued := session.resumes[request.AttachmentID]
	participant, attached := session.participants[request.AttachmentID]
	if !issued || !attached {
		return AttachResult{}, ErrInvalidInput
	}
	delete(session.resumes, request.AttachmentID)
	match := subtle.ConstantTimeCompare(record.secret[:], request.Secret) == 1
	if !match || participant.AccountID != request.AccountID || participant.ClientID != request.ClientID || !request.Standing && request.Resource != record.resource {
		if record.failures++; record.failures < maxResumeFailures {
			session.resumes[request.AttachmentID] = record
		}
		return AttachResult{}, ErrInvalidInput
	}
	return m.attachParticipantLocked(session, participant, request.FromSequence, 0)
}

func (m *RemoteManager) IssueResume(sessionID, attachmentID string, lease uint64, resource string) ([]byte, error) {
	var out struct{ V0 []byte }
	err := m.invoke(context.Background(), "IssueResume", struct {
		Sessionid    string
		Attachmentid string
		Lease        uint64
		Resource     string
	}{sessionID, attachmentID, lease, resource}, &out)
	return out.V0, err
}

func (m *RemoteManager) ResumeAttachment(sessionID string, request ResumeRequest) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "ResumeAttachment", struct {
		Sessionid string
		Request   ResumeRequest
	}{sessionID, request}, &out)
	return out.V0, err
}

func serveResume(m Service, op string, body json.RawMessage) (json.RawMessage, error) {
	if op == "IssueResume" {
		var args struct {
			Sessionid    string
			Attachmentid string
			Lease        uint64
			Resource     string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		secret, err := m.IssueResume(args.Sessionid, args.Attachmentid, args.Lease, args.Resource)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 []byte }{secret})
	}
	var args struct {
		Sessionid string
		Request   ResumeRequest
	}
	if err := decodeRemoteRequest(body, &args); err != nil {
		return nil, ErrInvalidSession
	}
	result, err := m.ResumeAttachment(args.Sessionid, args.Request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct{ V0 AttachResult }{result})
}
