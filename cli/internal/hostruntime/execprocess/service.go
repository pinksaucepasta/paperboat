package execprocess

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
)

// Service owns executions independently of a feature runtime's connections.
type Service interface {
	Start(context.Context, Request) (ExecutionService, bool, error)
	Get(string) (ExecutionService, error)
	ActiveSnapshots() []Snapshot
}

// UpdateAdmission lets the updater fence new executions until a replacement
// has committed or rolled back.
type UpdateAdmission interface {
	BeginUpdate(context.Context, string) error
	EndUpdate(string) error
}

type ExecutionService interface {
	Snapshot() Snapshot
	WaitStarted(context.Context) (Snapshot, error)
	Wait(context.Context) (Snapshot, error)
	Next(context.Context, uint64) (Event, error)
	OpenReader(uint64) (ReaderService, error)
	Write([]byte) (int, error)
	CloseInput() error
	Signal(pty.Signal) error
	Resize(pty.Dimensions) error
	Cancel(context.Context) error
}
type ReaderService interface {
	Next(context.Context) (Event, func(), error)
	Close() error
}
