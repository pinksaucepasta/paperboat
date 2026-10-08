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
type ExecutionService interface {
	Snapshot() Snapshot
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
