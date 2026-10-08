package session

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"sync"
	"time"
)

// RemoteCall is the private, authenticated terminal-owner transport.
type RemoteCall func(context.Context, string, json.RawMessage) (json.RawMessage, error)
type RemoteManager struct {
	call RemoteCall
	mu   sync.Mutex
	last error
}

func NewRemoteManager(call RemoteCall) (*RemoteManager, error) {
	if call == nil {
		return nil, ErrInvalidSession
	}
	return &RemoteManager{call: call}, nil
}
func (m *RemoteManager) LastError() error { m.mu.Lock(); defer m.mu.Unlock(); return m.last }
func (m *RemoteManager) invoke(ctx context.Context, op string, args, out any) error {
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	body, err := m.call(ctx, op, encoded)
	if err == nil {
		err = json.Unmarshal(body, out)
	}
	m.mu.Lock()
	m.last = err
	m.mu.Unlock()
	return err
}
func (m *RemoteManager) ResourceCounts() map[string]uint64 {
	var out struct{ V0 map[string]uint64 }
	err := m.invoke(context.Background(), "ResourceCounts", struct{}{}, &out)
	_ = err
	return out.V0
}
func (m *RemoteManager) Create(ctx context.Context, request CreateRequest) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(ctx, "Create", struct {
		Request            CreateRequest
		ManagedEnvironment []string
	}{request, LaunchEnvironment(ctx)}, &out)
	return out.V0, err
}

func (m *RemoteManager) BeginUpdate(transactionID string) error { return nil }
func (m *RemoteManager) EndUpdate(transactionID string) error   { return nil }
func (m *RemoteManager) Attach(sessionID, attachmentID string, fromSequence uint64) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "Attach", struct {
		Sessionid    string
		Attachmentid string
		Fromsequence uint64
	}{sessionID, attachmentID, fromSequence}, &out)
	return out.V0, err
}
func (m *RemoteManager) AttachParticipant(sessionID string, participant Participant, fromSequence uint64) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "AttachParticipant", struct {
		Sessionid    string
		Participant  remoteParticipant
		Fromsequence uint64
	}{sessionID, remoteParticipant{Participant: participant, Browser: participant.Browser}, fromSequence}, &out)
	return out.V0, err
}
func (m *RemoteManager) AttachParticipantAtGeneration(sessionID string, participant Participant, fromSequence, expectedGeneration uint64) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "AttachParticipantAtGeneration", struct {
		Sessionid          string
		Participant        remoteParticipant
		Fromsequence       uint64
		Expectedgeneration uint64
	}{sessionID, remoteParticipant{Participant: participant, Browser: participant.Browser}, fromSequence, expectedGeneration}, &out)
	return out.V0, err
}
func (m *RemoteManager) AttachLive(sessionID, attachmentID string) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "AttachLive", struct {
		Sessionid    string
		Attachmentid string
	}{sessionID, attachmentID}, &out)
	return out.V0, err
}
func (m *RemoteManager) AttachLiveParticipant(sessionID string, participant Participant) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "AttachLiveParticipant", struct {
		Sessionid   string
		Participant remoteParticipant
	}{sessionID, remoteParticipant{Participant: participant, Browser: participant.Browser}}, &out)
	return out.V0, err
}
func (m *RemoteManager) AttachLiveParticipantAtGeneration(sessionID string, participant Participant, expectedGeneration uint64) (AttachResult, error) {
	var out struct{ V0 AttachResult }
	err := m.invoke(context.Background(), "AttachLiveParticipantAtGeneration", struct {
		Sessionid          string
		Participant        remoteParticipant
		Expectedgeneration uint64
	}{sessionID, remoteParticipant{Participant: participant, Browser: participant.Browser}, expectedGeneration}, &out)
	return out.V0, err
}
func (m *RemoteManager) BrowserScreenAttachment(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64) ([]byte, uint64, error) {
	var out struct {
		V0 []byte
		V1 uint64
	}
	err := m.invoke(context.Background(), "BrowserScreenAttachment", struct {
		Sessionid          string
		Attachmentid       string
		Accountid          string
		Clientid           string
		Expectedgeneration uint64
	}{sessionID, attachmentID, accountID, clientID, expectedGeneration}, &out)
	return out.V0, out.V1, err
}
func (m *RemoteManager) Detach(sessionID, attachmentID string) error {
	var out struct{}
	err := m.invoke(context.Background(), "Detach", struct {
		Sessionid    string
		Attachmentid string
	}{sessionID, attachmentID}, &out)
	return err
}
func (m *RemoteManager) DetachParticipantAtGeneration(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64) error {
	var out struct{}
	err := m.invoke(context.Background(), "DetachParticipantAtGeneration", struct {
		Sessionid          string
		Attachmentid       string
		Accountid          string
		Clientid           string
		Expectedgeneration uint64
	}{sessionID, attachmentID, accountID, clientID, expectedGeneration}, &out)
	return err
}
func (m *RemoteManager) AttachmentOwnedBy(sessionID, attachmentID, accountID, clientID string) bool {
	var out struct{ V0 bool }
	err := m.invoke(context.Background(), "AttachmentOwnedBy", struct {
		Sessionid    string
		Attachmentid string
		Accountid    string
		Clientid     string
	}{sessionID, attachmentID, accountID, clientID}, &out)
	_ = err
	return out.V0
}
func (m *RemoteManager) Next(sessionID, attachmentID string) (history.Event, bool, error) {
	var out struct {
		V0 history.Event
		V1 bool
	}
	err := m.invoke(context.Background(), "Next", struct {
		Sessionid    string
		Attachmentid string
	}{sessionID, attachmentID}, &out)
	return out.V0, out.V1, err
}
func (m *RemoteManager) WaitNext(ctx context.Context, sessionID, attachmentID string) (history.Event, error) {
	var out struct{ V0 history.Event }
	err := m.invoke(ctx, "WaitNext", struct {
		Sessionid    string
		Attachmentid string
	}{sessionID, attachmentID}, &out)
	return out.V0, err
}
func (m *RemoteManager) Acknowledge(sessionID, attachmentID string, nextSequence uint64) error {
	var out struct{}
	err := m.invoke(context.Background(), "Acknowledge", struct {
		Sessionid    string
		Attachmentid string
		Nextsequence uint64
	}{sessionID, attachmentID, nextSequence}, &out)
	return err
}
func (m *RemoteManager) AcknowledgeAtGeneration(sessionID, attachmentID string, nextSequence, expectedGeneration uint64) error {
	var out struct{}
	err := m.invoke(context.Background(), "AcknowledgeAtGeneration", struct {
		Sessionid          string
		Attachmentid       string
		Nextsequence       uint64
		Expectedgeneration uint64
	}{sessionID, attachmentID, nextSequence, expectedGeneration}, &out)
	return err
}
func (m *RemoteManager) AttachmentStatus(sessionID, attachmentID string) (AttachmentState, uint64, error) {
	var out struct {
		V0 AttachmentState
		V1 uint64
	}
	err := m.invoke(context.Background(), "AttachmentStatus", struct {
		Sessionid    string
		Attachmentid string
	}{sessionID, attachmentID}, &out)
	return out.V0, out.V1, err
}
func (m *RemoteManager) Write(sessionID string, key InputKey, data []byte) (InputDecision, error) {
	var out struct{ V0 InputDecision }
	err := m.invoke(context.Background(), "Write", struct {
		Sessionid string
		Key       InputKey
		Data      []byte
	}{sessionID, key, data}, &out)
	return out.V0, err
}
func (m *RemoteManager) InputSequence(sessionID, clientID, attachmentID string, generation uint64) (uint64, error) {
	var out struct{ V0 uint64 }
	err := m.invoke(context.Background(), "InputSequence", struct {
		Sessionid    string
		Clientid     string
		Attachmentid string
		Generation   uint64
	}{sessionID, clientID, attachmentID, generation}, &out)
	return out.V0, err
}
func (m *RemoteManager) WriteStream(sessionID, attachmentID string, generation uint64, data []byte) error {
	var out struct{}
	err := m.invoke(context.Background(), "WriteStream", struct {
		Sessionid    string
		Attachmentid string
		Generation   uint64
		Data         []byte
	}{sessionID, attachmentID, generation, data}, &out)
	return err
}
func (m *RemoteManager) QueryInput(sessionID string, key InputKey) (InputDecision, error) {
	var out struct{ V0 InputDecision }
	err := m.invoke(context.Background(), "QueryInput", struct {
		Sessionid string
		Key       InputKey
	}{sessionID, key}, &out)
	return out.V0, err
}
func (m *RemoteManager) Resize(sessionID, attachmentID string, dimensions pty.Dimensions, activeAt time.Time) error {
	var out struct{}
	err := m.invoke(context.Background(), "Resize", struct {
		Sessionid    string
		Attachmentid string
		Dimensions   pty.Dimensions
		Activeat     time.Time
	}{sessionID, attachmentID, dimensions, activeAt}, &out)
	return err
}
func (m *RemoteManager) ResizeParticipantAtGeneration(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64, dimensions pty.Dimensions, activeAt time.Time) error {
	var out struct{}
	err := m.invoke(context.Background(), "ResizeParticipantAtGeneration", struct {
		Sessionid          string
		Attachmentid       string
		Accountid          string
		Clientid           string
		Expectedgeneration uint64
		Dimensions         pty.Dimensions
		Activeat           time.Time
	}{sessionID, attachmentID, accountID, clientID, expectedGeneration, dimensions, activeAt}, &out)
	return err
}
func (m *RemoteManager) Signal(sessionID string, generation uint64, signal pty.Signal) error {
	var out struct{}
	err := m.invoke(context.Background(), "Signal", struct {
		Sessionid  string
		Generation uint64
		Signal     pty.Signal
	}{sessionID, generation, signal}, &out)
	return err
}
func (m *RemoteManager) Clear(sessionID string) (uint64, error) {
	var out struct{ V0 uint64 }
	err := m.invoke(context.Background(), "Clear", struct{ Sessionid string }{sessionID}, &out)
	return out.V0, err
}
func (m *RemoteManager) ClearAtGeneration(sessionID string, expectedGeneration uint64) (uint64, error) {
	var out struct{ V0 uint64 }
	err := m.invoke(context.Background(), "ClearAtGeneration", struct {
		Sessionid          string
		Expectedgeneration uint64
	}{sessionID, expectedGeneration}, &out)
	return out.V0, err
}
func (m *RemoteManager) Close(ctx context.Context, sessionID string) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(ctx, "Close", struct{ Sessionid string }{sessionID}, &out)
	return out.V0, err
}
func (m *RemoteManager) CloseAtGeneration(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(ctx, "CloseAtGeneration", struct {
		Sessionid          string
		Expectedgeneration uint64
	}{sessionID, expectedGeneration}, &out)
	return out.V0, err
}
func (m *RemoteManager) Restart(id string) (Snapshot, error) {
	return m.RestartContext(context.Background(), id)
}
func (m *RemoteManager) RestartAtGeneration(id string, generation uint64) (Snapshot, error) {
	return m.RestartAtGenerationContext(context.Background(), id, generation)
}
func (m *RemoteManager) RestartContext(ctx context.Context, id string) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(ctx, "RestartContext", struct {
		Sessionid          string
		ManagedEnvironment []string
	}{id, LaunchEnvironment(ctx)}, &out)
	return out.V0, err
}
func (m *RemoteManager) RestartAtGenerationContext(ctx context.Context, id string, generation uint64) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(ctx, "RestartAtGenerationContext", struct {
		Sessionid          string
		Expectedgeneration uint64
		ManagedEnvironment []string
	}{id, generation, LaunchEnvironment(ctx)}, &out)
	return out.V0, err
}
func (m *RemoteManager) Delete(sessionID string) error {
	var out struct{}
	err := m.invoke(context.Background(), "Delete", struct{ Sessionid string }{sessionID}, &out)
	return err
}
func (m *RemoteManager) DeleteAtGeneration(sessionID string, expectedGeneration uint64) error {
	var out struct{}
	err := m.invoke(context.Background(), "DeleteAtGeneration", struct {
		Sessionid          string
		Expectedgeneration uint64
	}{sessionID, expectedGeneration}, &out)
	return err
}
func (m *RemoteManager) Snapshot(sessionID string) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(context.Background(), "Snapshot", struct{ Sessionid string }{sessionID}, &out)
	return out.V0, err
}
func (m *RemoteManager) SnapshotAtGeneration(sessionID string, expectedGeneration uint64) (Snapshot, error) {
	var out struct{ V0 Snapshot }
	err := m.invoke(context.Background(), "SnapshotAtGeneration", struct {
		Sessionid          string
		Expectedgeneration uint64
	}{sessionID, expectedGeneration}, &out)
	return out.V0, err
}
func (m *RemoteManager) List() []Snapshot {
	var out struct{ V0 []Snapshot }
	err := m.invoke(context.Background(), "List", struct{}{}, &out)
	_ = err
	return out.V0
}
func (m *RemoteManager) Shutdown(ctx context.Context) error            { return nil }
func (m *RemoteManager) ShutdownForRecovery(ctx context.Context) error { return nil }

var ErrOwnerUnavailable = errors.New("terminal owner unavailable")

// Browser is intentionally absent from public participant JSON. It is required
// across the private owner boundary for live screen authorization.
type remoteParticipant struct {
	Participant
	Browser bool
}

func (p remoteParticipant) participant() Participant {
	v := p.Participant
	v.Browser = p.Browser
	return v
}
