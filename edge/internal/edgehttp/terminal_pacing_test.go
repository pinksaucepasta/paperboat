package edgehttp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type forwarded struct {
	size int
	at   time.Time
}

// A capped terminal stays responsive: while bulk output waits for its workspace's tokens, an interactive frame (input echo, resize)
// behind it is forwarded at once (within 100 ms of arriving), and the bulk it released early is repaid by the bulk that follows, so
// the workspace's average rate still holds.
func TestCappedTerminalKeepsInteractiveFramesResponsiveAndHoldsTheRate(t *testing.T) {
	throttles := newThrottles()
	const host = "machine.runtime.example.test"
	throttles.Set([]connectorprotocol.IngressThrottle{{Hostname: host, Workspace: "personal:one", BytesPerSecond: 128 << 10, Terminal: true}})
	var mu sync.Mutex
	var out []forwarded
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	in, done := terminalPacing{throttles: throttles, host: host}.start(ctx, &workers, func(err error) { t.Errorf("pacing failed: %v", err) }, func(frame []byte) error {
		mu.Lock()
		out = append(out, forwarded{len(frame), time.Now()})
		mu.Unlock()
		return nil
	})
	started := time.Now()
	bulk := make([]byte, 16<<10)
	go func() {
		// Eight 16 KiB bulk frames at once (a program dumping output), then, while the pacer waits on them, interactive frames.
		for range 8 {
			in <- bulk
		}
	}()
	var echoSent []time.Time
	for i := range 4 {
		time.Sleep(200 * time.Millisecond)
		echoSent = append(echoSent, time.Now())
		in <- make([]byte, 40+i) // an echoed keystroke or a resize
	}
	// More bulk follows: what was released early for the echoes is repaid here, so the whole dump still runs at the throttled rate.
	for range 8 {
		in <- bulk
	}
	close(in)
	<-done
	workers.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(out) != 20 {
		t.Fatalf("forwarded %d frames, want 20", len(out))
	}
	var small []forwarded
	var bulkBytes int
	var lastBulk time.Time
	for _, f := range out {
		if f.size < terminalInteractiveBytes {
			small = append(small, f)
		} else {
			bulkBytes += f.size
			lastBulk = f.at
		}
	}
	if len(small) != 4 {
		t.Fatalf("forwarded %d interactive frames, want 4", len(small))
	}
	for i, f := range small {
		if delay := f.at.Sub(echoSent[i]); delay > 100*time.Millisecond {
			t.Fatalf("interactive frame %d waited %v behind bulk output, want under 100 ms", i, delay)
		}
	}
	// 256 KiB at 128 KiB/s with a 32 KiB burst takes at least 1.75 s: the bulk released early for the interactive frames is repaid.
	if elapsed := lastBulk.Sub(started); bulkBytes != 256<<10 || elapsed < 1500*time.Millisecond {
		t.Fatalf("%d bytes of bulk output passed in %v, faster than the throttled rate allows", bulkBytes, elapsed)
	}
}

// Without a cap for the terminal's host nothing waits: frames pass as they come.
func TestUnthrottledTerminalIsNotPaced(t *testing.T) {
	throttles := newThrottles()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	var count int
	var mu sync.Mutex
	in, done := terminalPacing{throttles: throttles, host: "machine.runtime.example.test"}.start(ctx, &workers, func(err error) { t.Errorf("pacing failed: %v", err) }, func([]byte) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})
	started := time.Now()
	for range 20 {
		in <- make([]byte, 64<<10)
	}
	close(in)
	<-done
	workers.Wait()
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond || count != 20 {
		t.Fatalf("%d of 20 frames in %v, want all at once", count, elapsed)
	}
}

// Two sessions of one workspace share ONE bucket: opening a second tab does not double the cap. Both push 128 KiB at once through a
// 128 KiB/s cap (32 KiB burst); alone either would take about 0.75 s, together they take about 1.75 s. A session of another workspace
// is not slowed by them, and the workspace's allowance throttle (same workspace, not a terminal entry) is a bucket of its own.
func TestTerminalSessionsOfAWorkspaceShareOneCapBucket(t *testing.T) {
	throttles := newThrottles()
	throttles.Set([]connectorprotocol.IngressThrottle{
		{Hostname: "a.runtime.example.test", Workspace: "personal:one", BytesPerSecond: 128 << 10, Terminal: true},
		{Hostname: "b.runtime.example.test", Workspace: "personal:one", BytesPerSecond: 128 << 10, Terminal: true},
		{Hostname: "c.runtime.example.test", Workspace: "personal:two", BytesPerSecond: 128 << 10, Terminal: true},
		{Hostname: "preview.example.test", Workspace: "personal:one", BytesPerSecond: 1 << 10},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(host string, frames int) (time.Duration, int) {
		var workers sync.WaitGroup
		var count int
		in, done := terminalPacing{throttles: throttles, host: host}.start(ctx, &workers, func(err error) { t.Errorf("pacing failed: %v", err) }, func([]byte) error {
			count++
			return nil
		})
		started := time.Now()
		for range frames {
			in <- make([]byte, 16<<10)
		}
		close(in)
		<-done
		workers.Wait()
		return time.Since(started), count
	}
	var wg sync.WaitGroup
	var elapsedA, elapsedB time.Duration
	wg.Add(2)
	go func() { defer wg.Done(); elapsedA, _ = run("a.runtime.example.test", 8) }()
	go func() { defer wg.Done(); elapsedB, _ = run("b.runtime.example.test", 8) }()
	wg.Wait()
	if total := max(elapsedA, elapsedB); total < 1500*time.Millisecond {
		t.Fatalf("two sessions pushed 256 KiB through a 128 KiB/s cap in %v: the sessions do not share one bucket", total)
	}
	if other, _ := run("c.runtime.example.test", 2); other > 500*time.Millisecond {
		t.Fatalf("a session of another workspace waited %v behind the first workspace's cap", other)
	}
	// The allowance's 1 KiB/s throttle on the same workspace does not touch the terminals: the runs above would never finish otherwise.
}

// A key frame is not queued behind a bulk upload: bulk frames the browser sends (a paste, an upload) wait for tokens in the same pacer,
// and a keystroke arriving behind them is forwarded at once.
func TestKeystrokeIsNotQueuedBehindABulkUpload(t *testing.T) {
	throttles := newThrottles()
	const host = "machine.runtime.example.test"
	throttles.Set([]connectorprotocol.IngressThrottle{{Hostname: host, Workspace: "personal:one", BytesPerSecond: 64 << 10, Terminal: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	arrived := make(chan time.Time, 1)
	var key time.Time
	in, done := terminalPacing{throttles: throttles, host: host}.start(ctx, &workers, func(err error) { t.Errorf("pacing failed: %v", err) }, func(frame []byte) error {
		if len(frame) == 3 {
			arrived <- time.Now()
		}
		return nil
	})
	go func() {
		for range 6 {
			in <- make([]byte, 32<<10) // an upload far above the cap: seconds of tokens
		}
	}()
	time.Sleep(300 * time.Millisecond)
	key = time.Now()
	in <- []byte("ls\n")
	select {
	case at := <-arrived:
		if delay := at.Sub(key); delay > 100*time.Millisecond {
			t.Fatalf("a keystroke waited %v behind the upload, want under 100 ms", delay)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a keystroke was queued behind the bulk upload")
	}
	cancel()
	workers.Wait()
	_ = done
}
