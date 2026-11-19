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
			in <- terminalFrame{payload: bulk}
		}
	}()
	var echoSent []time.Time
	for i := range 4 {
		time.Sleep(200 * time.Millisecond)
		echoSent = append(echoSent, time.Now())
		in <- terminalFrame{payload: make([]byte, 40+i)} // an echoed keystroke or a resize
	}
	// More bulk follows: what was released early for the echoes is repaid here, so the whole dump still runs at the throttled rate.
	for range 8 {
		in <- terminalFrame{payload: bulk}
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
		in <- terminalFrame{payload: make([]byte, 64<<10)}
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
			in <- terminalFrame{payload: make([]byte, 16<<10)}
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
			in <- terminalFrame{payload: make([]byte, 32<<10)} // an upload far above the cap: seconds of tokens
		}
	}()
	time.Sleep(300 * time.Millisecond)
	key = time.Now()
	in <- terminalFrame{payload: []byte("ls\n")}
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

// A file transfer through the terminal counts toward the workspace's allowance and is slowed by it past the allowance; typing is
// neither counted nor slowed by it. The browser marks the records of its file-transfer requests (the edge cannot read the encrypted
// stream); the allowance's bucket is a separate entry from the terminal's cap, so only marked frames meet it.
func TestTerminalFileTransferCountsAndIsSlowedPastTheAllowanceTypingIsNot(t *testing.T) {
	throttles := newThrottles()
	const host = "machine.runtime.example.test"
	throttles.Set([]connectorprotocol.IngressThrottle{
		{Hostname: host, Workspace: "personal:one", BytesPerSecond: 10 << 20, Terminal: true}, // the anti-abuse cap, far above the test's rate
		{Hostname: "files:" + host, Workspace: "personal:one", BytesPerSecond: 64 << 10},      // the workspace past its allowance
	})
	run := func(frames []terminalFrame) (elapsed time.Duration, counted int) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var workers sync.WaitGroup
		pacing := terminalPacing{throttles: throttles, host: host, fileHost: "files:" + host, fileBytes: func(n int) { counted += n }}
		in, done := pacing.start(ctx, &workers, func(err error) { t.Errorf("pacing failed: %v", err) }, func([]byte) error { return nil })
		started := time.Now()
		for _, f := range frames {
			in <- f
		}
		close(in)
		<-done
		workers.Wait()
		return time.Since(started), counted
	}
	// 192 KiB of marked upload records at 64 KiB/s (32 KiB burst): at least 2.5 s, and every byte counted.
	var upload []terminalFrame
	for range 12 {
		upload = append(upload, terminalFrame{payload: make([]byte, 16<<10), file: true})
	}
	elapsed, counted := run(upload)
	if counted != 192<<10 || elapsed < 2*time.Second {
		t.Fatalf("upload: %d bytes counted in %v, want all 192 KiB counted and slowed to the allowance's rate", counted, elapsed)
	}
	// Typing and output (unmarked, however large) are never counted and never meet the allowance's bucket.
	var typing []terminalFrame
	for range 12 {
		typing = append(typing, terminalFrame{payload: make([]byte, 16<<10)}, terminalFrame{payload: []byte("ls\n")})
	}
	elapsed, counted = run(typing)
	if counted != 0 || elapsed > 500*time.Millisecond {
		t.Fatalf("typing and output: %d bytes counted in %v, want none counted and no slowdown", counted, elapsed)
	}
}

// The edge tells the browser's file-transfer records from the rest by the marker byte alone and strips it like the TLS marker.
func TestBrowserTerminalFileMarkerIsRecognised(t *testing.T) {
	if record, file, err := browserTerminalTLSMessagePayload([]byte{browserTerminalFileDiscriminator, 7, 8}); err != nil || !file || string(record) != string([]byte{7, 8}) {
		t.Fatalf("file record = %v %v %v", record, file, err)
	}
	if record, file, err := browserTerminalTLSMessagePayload([]byte{browserTerminalTLSDiscriminator, 7, 8}); err != nil || file || string(record) != string([]byte{7, 8}) {
		t.Fatalf("plain record = %v %v %v", record, file, err)
	}
	if _, _, err := browserTerminalTLSMessagePayload([]byte{browserTerminalSharedOutputDiscriminator, 7}); err == nil {
		t.Fatal("a shared-output marker was accepted from the browser")
	}
}
