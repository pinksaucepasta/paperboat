package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/tunnel"
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

// hop-89 / a Windows test VM pb install: the first exec while the peer restarted (worker unavailable 41 s) gave up after three
// 10 s opens with exec_start_uncertain. The start is now retried with the same operation ID for the reattach window, so
// the exec runs once the machine answers, and only once.
func TestFirstExecAfterPeerRestartRunsOnceWithTheSameOperation(t *testing.T) {
	host := &restartingHost{down: 4, starts: map[string]int{}, attached: map[string]bool{}}
	const operation = "operation_restart"
	_, err := host.open(operation)
	if err == nil || !execStartUncertain(err) {
		t.Fatalf("first open while restarting: %v", err)
	}
	conn, err := retryUncertainExecStart(context.Background(), func(context.Context) (tunnel.ExecConn, error) {
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
// Checked on the deadline the opens are handed, not on how long the run took (a loaded runner makes wall time
// meaningless): the window is the bound only if every open gets one deadline, no earlier than the window from the
// call and no later than the window from the moment the first open ran.
func TestUncertainExecStartEndsAtTheWindowStillUncertain(t *testing.T) {
	const window = 300 * time.Millisecond
	ctx := withExecReattach(context.Background(), 0, window)
	begin := time.Now()
	var deadlines []time.Time
	var firstRan time.Time
	_, err := retryUncertainExecStart(ctx, func(ctx context.Context) (tunnel.ExecConn, error) {
		deadline, bounded := ctx.Deadline()
		if !bounded {
			t.Error("an open without a deadline: the window does not bound it")
		}
		if deadlines = append(deadlines, deadline); len(deadlines) == 1 {
			firstRan = time.Now()
			return nil, localapi.ErrExecStartUncertain
		}
		<-ctx.Done() // a silent machine: the window cancels this open before the daemon's own timeout
		return nil, ctx.Err()
	})
	end := time.Now()
	if !execStartUncertain(err) || len(deadlines) != 2 {
		t.Fatalf("err=%v after %d opens", err, len(deadlines))
	}
	if deadlines[0] != deadlines[1] || deadlines[0].Before(begin.Add(window)) || deadlines[0].After(firstRan.Add(window)) {
		t.Fatalf("open deadlines %v, want one deadline in [%v, %v]", deadlines, begin.Add(window), firstRan.Add(window))
	}
	if end.Before(deadlines[0]) {
		t.Fatalf("returned at %v, before the window's deadline %v", end, deadlines[0])
	}
}

// W1: `pb exec --timeout 2s` against a machine in the middle of an update waited 10 s, because the uncertain-start
// recovery (up to 90 s) ignored the user's timeout. The timeout bounds the start too: the exec returns at the
// timeout, still reporting that the start is uncertain, whether the opens keep failing or the first one never answers.
// Checked on the deadline every open is handed, not on how long the run took: a loaded runner makes wall time
// meaningless, while a recovery that outlives the timeout shows as an open with a later (or no) deadline.
func TestUncertainStartEndsAtTheUserTimeout(t *testing.T) {
	const timeout = 2 * time.Second
	for name, open := range map[string]func(context.Context) (tunnel.ExecConn, error){
		"opens keep failing": func(ctx context.Context) (tunnel.ExecConn, error) {
			select {
			case <-time.After(300 * time.Millisecond): // the daemon reports an open that did not complete
				return nil, localapi.ErrExecStartUncertain
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		"first open never answers": func(ctx context.Context) (tunnel.ExecConn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var first time.Time
			var opens int
			var outlived string
			begin := time.Now()
			conn, uncertain, err := startExec(context.Background(), timeout, func(ctx context.Context) (tunnel.ExecConn, error) {
				deadline, bounded := ctx.Deadline()
				mu.Lock()
				defer mu.Unlock()
				if opens++; opens == 1 {
					first = deadline
				}
				if !bounded || deadline.After(first) {
					outlived = fmt.Sprintf("open %d got deadline %v (bounded=%v), after the timeout's %v", opens, deadline, bounded, first)
				}
				return open(ctx)
			})
			mu.Lock()
			defer mu.Unlock()
			if conn != nil || !uncertain || !execStartUncertain(err) {
				t.Fatalf("conn %v uncertain %v err %v", conn, uncertain, err)
			}
			if outlived != "" {
				t.Fatal(outlived)
			}
			if first.Before(begin.Add(timeout)) {
				t.Fatalf("the start's deadline %v is shorter than --timeout %v from %v", first, timeout, begin)
			}
			if time.Now().Before(first) {
				t.Fatalf("returned before the --timeout deadline %v", first)
			}
		})
	}
}
