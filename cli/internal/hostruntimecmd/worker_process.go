//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// workerStartRequest is fixed native hostd startup data, never update policy.
// The owner supplies the executable, OS identity, API range and sealed capability.
type workerStartRequest struct {
	Executable        string
	Version           string
	WorkerID          string
	UID               int
	GID               int
	APIMin            uint16
	APIMax            uint16
	HostdEndpoint     string
	Capability        []byte
	MutationsDisabled bool
	// Listener is the loopback runtime listener hostd owns; every worker
	// inherits it so the port never closes while workers or hostd change.
	Listener *os.File
}

type workerProcess interface {
	Ready(context.Context) (hostdproto.Status, error)
	Activate(context.Context) (hostdproto.Status, error)
	Stop(context.Context) error
}

// exitWatcher is implemented by worker processes that report their own exit.
// Hostd uses it to notice a runtime worker that died without being stopped.
type exitWatcher interface {
	Exited() <-chan struct{}
	// ExitReason is a bounded, non-secret description of how the process ended.
	ExitReason() string
}

// drainer is implemented by worker processes that can finish in-flight
// requests before exiting, used when a new hostd generation took over.
type drainer interface {
	Drain(context.Context) error
}
