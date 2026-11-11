package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// restartingHost answers exec opens like a machine updating in place (same node key): its first opens time out, as
// the daemon reports a stream that did not open in 10 s, then it is back. It runs each operation ID at most once.
type restartingHost struct {
	mu       sync.Mutex
	down     int
	opens    []string
	starts   map[string]int
	attached map[string]bool
}

func (h *restartingHost) open(operationID string) (tunnel.ExecConn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens = append(h.opens, operationID)
	if len(h.opens) <= h.down {
		return nil, localapi.ErrExecStartUncertain
	}
	if !h.attached[operationID] {
		h.attached[operationID] = true
		h.starts[operationID]++
	}
	return &cutConn{host: &hostStdin{}}, nil
}

// hop-89 / winpool pb install: the first exec while the peer restarted (worker unavailable 41 s) gave up after three
// 10 s opens with exec_start_uncertain. The start is now retried with the same operation ID for the reattach window, so
// the exec runs once the machine answers, and only once.
func TestFirstExecAfterPeerRestartRunsOnceWithTheSameOperation(t *testing.T) {
	host := &restartingHost{down: 4, starts: map[string]int{}, attached: map[string]bool{}}
	const operation = "operation_restart"
	conn, err := host.open(operation)
	if err == nil || !execStartUncertain(err) {
		t.Fatalf("first open while restarting: %v", err)
	}
	conn, err = retryUncertainExecStart(context.Background(), func(context.Context) (tunnel.ExecConn, error) {
		return host.open(operation)
	})
	if err != nil || conn == nil {
		t.Fatalf("exec after the peer restarted: %v", err)
	}
	for _, id := range host.opens {
		if id != operation {
			t.Fatalf("a retry used operation %q", id)
		}
	}
	if host.starts[operation] != 1 || len(host.opens) != host.down+1 {
		t.Fatalf("starts %d, opens %d", host.starts[operation], len(host.opens))
	}
}

// A machine that does not come back within the window still reports start-uncertain (the host may have the command),
// not a generic "Paperboat is unreachable", and the wait ends at the window.
func TestUncertainExecStartEndsAtTheWindowStillUncertain(t *testing.T) {
	old := execReattachWindow
	execReattachWindow = 300 * time.Millisecond
	defer func() { execReattachWindow = old }()
	start := time.Now()
	opens := 0
	_, err := retryUncertainExecStart(context.Background(), func(ctx context.Context) (tunnel.ExecConn, error) {
		if opens++; opens == 1 {
			return nil, localapi.ErrExecStartUncertain
		}
		<-ctx.Done() // a silent machine: the window cancels this open before the daemon's own timeout
		return nil, ctx.Err()
	})
	if !execStartUncertain(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
