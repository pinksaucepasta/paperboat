//go:build darwin || linux

package localapi

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type failedWatchConnection struct {
	net.Conn
	setup bool
}

func (connection failedWatchConnection) SyscallConn() (syscall.RawConn, error) {
	if connection.setup {
		return nil, syscall.EIO
	}
	return failedWatchRaw{}, nil
}

type failedWatchRaw struct{}

func (failedWatchRaw) Control(func(uintptr)) error    { return syscall.EIO }
func (failedWatchRaw) Read(func(uintptr) bool) error  { return syscall.EIO }
func (failedWatchRaw) Write(func(uintptr) bool) error { return syscall.EIO }

func TestPeerHangupWatcherFailureCancelsLeaseWithCorrelatedCause(t *testing.T) {
	for _, setup := range []bool{true, false} {
		left, right := net.Pipe()
		reference := supportref.New()
		ctx, cancel := context.WithCancel(supportref.WithContext(t.Context(), reference))
		var faults []errorreport.Fault
		restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
		watchPeerHangup(ctx, failedWatchConnection{Conn: left, setup: setup}, Peer{}, cancel)
		restore()
		_ = left.Close()
		_ = right.Close()
		if ctx.Err() != context.Canceled {
			t.Fatal("failed hangup watcher left its lease active")
		}
		if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference || faults[0].Stage != "local_gateway" {
			t.Fatalf("watcher faults=%#v", faults)
		}
		cancel()
	}
}
