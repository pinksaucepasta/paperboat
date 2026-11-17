//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
)

var errWorkerSilent = errors.New("runtime worker stopped sending heartbeats")

const workerLivenessInterval = 5 * time.Second

// workerFrozenAfter is well past the longest gap a serving worker leaves between
// heartbeats (a 30 s bridge health call, its retry, then the 15 s Windows heartbeat: 75 s).
const workerFrozenAfter = 2 * time.Minute

// workerLiveness reports an active worker whose heartbeat has not advanced for
// workerFrozenAfter of hostd's own (monotonic) running time. A suspended machine
// resumes both processes, so the heartbeat moves again before that.
type workerLiveness struct {
	epoch     uint64
	heartbeat int64
	since     time.Time
}

func (l *workerLiveness) silent(status hostdproto.Status, now time.Time) bool {
	if status.State != hostdproto.StateActive || l.since.IsZero() || status.Epoch != l.epoch || status.LastHeartbeatUnixMilli != l.heartbeat {
		*l = workerLiveness{epoch: status.Epoch, heartbeat: status.LastHeartbeatUnixMilli, since: now}
		return false
	}
	return now.Sub(l.since) > workerFrozenAfter
}

// stackDumper is implemented by worker processes whose goroutine stacks hostd
// can capture from outside; a frozen worker cannot be asked over its own IPC.
type stackDumper interface {
	DumpStacks(context.Context) (string, error)
}

// dumpActiveStacks writes the active worker's goroutine stacks to sink before
// hostd restarts it, so the next freeze names its location. Windows has no
// external goroutine-dump signal: it records that limit instead.
func (o *ownedWorkers) dumpActiveStacks(ctx context.Context, sink io.Writer) {
	o.mu.Lock()
	var process workerProcess
	if w := o.workers[o.active]; w != nil {
		process = w.process
	}
	o.mu.Unlock()
	dumper, ok := process.(stackDumper)
	if !ok {
		fmt.Fprintln(sink, "pb hostd: the frozen runtime worker's stacks cannot be captured on this platform")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stacks, err := dumper.DumpStacks(ctx)
	fmt.Fprintf(sink, "pb hostd: the runtime worker stopped sending heartbeats; its goroutine stacks follow (capture error: %v)\n%s\n", err, stacks)
}
