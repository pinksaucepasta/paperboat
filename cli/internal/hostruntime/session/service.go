package session

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"time"
)

// Service is the terminal boundary shared by the process owner and feature runtime.
// Shutdown on a remote service disconnects the runtime, never the owned processes.
type Service interface {
	ResourceCounts() map[string]uint64
	Create(ctx context.Context, request CreateRequest) (Snapshot, error)
	BeginUpdate(transactionID string) error
	EndUpdate(transactionID string) error
	Attach(sessionID, attachmentID string, fromSequence uint64) (AttachResult, error)
	AttachParticipant(sessionID string, participant Participant, fromSequence uint64) (AttachResult, error)
	AttachParticipantAtGeneration(sessionID string, participant Participant, fromSequence, expectedGeneration uint64) (AttachResult, error)
	AttachLive(sessionID, attachmentID string) (AttachResult, error)
	AttachLiveParticipant(sessionID string, participant Participant) (AttachResult, error)
	AttachLiveParticipantAtGeneration(sessionID string, participant Participant, expectedGeneration uint64) (AttachResult, error)
	BrowserScreenAttachment(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64) ([]byte, uint64, error)
	Detach(sessionID, attachmentID string) error
	DetachParticipantAtGeneration(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64) error
	AttachmentOwnedBy(sessionID, attachmentID, accountID, clientID string) bool
	Next(sessionID, attachmentID string) (history.Event, bool, error)
	WaitNext(ctx context.Context, sessionID, attachmentID string) (history.Event, error)
	Acknowledge(sessionID, attachmentID string, nextSequence uint64) error
	AcknowledgeAtGeneration(sessionID, attachmentID string, nextSequence, expectedGeneration uint64) error
	AttachmentStatus(sessionID, attachmentID string) (AttachmentState, uint64, error)
	Write(sessionID string, key InputKey, data []byte) (InputDecision, error)
	InputSequence(sessionID, clientID, attachmentID string, generation uint64) (uint64, error)
	WriteStream(sessionID, attachmentID string, generation uint64, data []byte) error
	QueryInput(sessionID string, key InputKey) (InputDecision, error)
	Resize(sessionID, attachmentID string, dimensions pty.Dimensions, activeAt time.Time) error
	ResizeParticipantAtGeneration(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64, dimensions pty.Dimensions, activeAt time.Time) error
	Signal(sessionID string, generation uint64, signal pty.Signal) error
	Clear(sessionID string) (uint64, error)
	ClearAtGeneration(sessionID string, expectedGeneration uint64) (uint64, error)
	Close(ctx context.Context, sessionID string) (Snapshot, error)
	CloseAtGeneration(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error)
	Restart(sessionID string) (Snapshot, error)
	RestartAtGeneration(sessionID string, expectedGeneration uint64) (Snapshot, error)
	RestartContext(ctx context.Context, sessionID string) (Snapshot, error)
	RestartAtGenerationContext(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error)
	Delete(sessionID string) error
	DeleteAtGeneration(sessionID string, expectedGeneration uint64) error
	Snapshot(sessionID string) (Snapshot, error)
	SnapshotAtGeneration(sessionID string, expectedGeneration uint64) (Snapshot, error)
	List() []Snapshot
	Shutdown(ctx context.Context) error
	ShutdownForRecovery(ctx context.Context) error
}
