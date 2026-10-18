package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStreamMatcherFindsFragmentedMarker(t *testing.T) {
	chunks := make(chan []byte, 3)
	done := make(chan error, 1)
	chunks <- []byte("discard-PB")
	chunks <- []byte("RTT0000")
	chunks <- []byte("0001Q-tail")
	matcher := newStreamMatcher(chunks, done)
	if err := matcher.wait(context.Background(), "PBRTT00000001Q"); err != nil {
		t.Fatal(err)
	}
}

func TestOutputIntervalsSummarizeBoundedArrivalTiming(t *testing.T) {
	intervals := &outputIntervals{}
	start := time.Unix(1, 0)
	for _, offset := range []time.Duration{0, time.Millisecond, 3 * time.Millisecond, 7 * time.Millisecond} {
		intervals.observe(start.Add(offset))
	}
	count, p50, p95, p99, mean := intervals.summarize()
	if count != 3 || p50 != 2 || p95 != 4 || p99 != 4 || mean != 2.333333 {
		t.Fatalf("summary = count:%d p50:%v p95:%v p99:%v mean:%v", count, p50, p95, p99, mean)
	}
}

func TestStreamMatcherFindsCursorAddressedMarker(t *testing.T) {
	chunks := make(chan []byte, 2)
	done := make(chan error, 1)
	chunks <- []byte("noise\x1b[2;1HPB\x1b[2;3H12")
	chunks <- []byte("\x1b[2;5H34Q\x1b[3;1H")
	matcher := newStreamMatcher(chunks, done)
	if err := matcher.wait(context.Background(), "PB1234Q"); err != nil {
		t.Fatal(err)
	}
}

func TestStreamMatcherFindsMarkerOverwrittenInSameChunk(t *testing.T) {
	chunks := make(chan []byte, 1)
	done := make(chan error, 1)
	chunks <- []byte("PB1234Q\x1b[1;1HXX")
	matcher := newStreamMatcher(chunks, done)
	if err := matcher.wait(context.Background(), "PB1234Q"); err != nil {
		t.Fatal(err)
	}
}

func TestStreamMatcherDoesNotJoinRows(t *testing.T) {
	chunks := make(chan []byte, 1)
	done := make(chan error, 1)
	chunks <- []byte("PB12\r\n34Q")
	done <- io.EOF
	matcher := newStreamMatcher(chunks, done)
	if err := matcher.wait(context.Background(), "PB1234Q"); err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
}

func TestStreamMatcherFindsMarkerWrappedAtScreenEdge(t *testing.T) {
	chunks := make(chan []byte, 1)
	done := make(chan error, 1)
	chunks <- []byte("\x1b[1;77HPB1234Q")
	matcher := newStreamMatcher(chunks, done)
	if err := matcher.wait(context.Background(), "PB1234Q"); err != nil {
		t.Fatal(err)
	}
}

func TestStreamMatcherReportsClosedOutput(t *testing.T) {
	chunks := make(chan []byte)
	done := make(chan error, 1)
	done <- io.EOF
	close(chunks)
	if err := newStreamMatcher(chunks, done).wait(context.Background(), "missing"); err != io.EOF {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamMatcherTimeoutDiagnosticsExcludeScreenContents(t *testing.T) {
	chunks := make(chan []byte, 1)
	done := make(chan error, 1)
	chunks <- []byte("PB12-secret-terminal-content")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	err := newStreamMatcher(chunks, done).wait(ctx, "PB1234Q")
	if err == nil || !strings.Contains(err.Error(), "marker_prefix=4") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunPrefixPreventsDurableReplayMarkerReuse(t *testing.T) {
	first, err := randomRunPrefix(bytes.NewReader(bytes.Repeat([]byte{0x11}, 4)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomRunPrefix(bytes.NewReader(bytes.Repeat([]byte{0x22}, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first != "11111111" || second != "22222222" {
		t.Fatalf("prefixes = %q, %q", first, second)
	}
}

func TestWaitUntilReadyDoesNotQueueDuplicateCommands(t *testing.T) {
	chunks := make(chan []byte, 1)
	done := make(chan error, 1)
	var writes bytes.Buffer
	go func() {
		time.Sleep(15 * time.Millisecond)
		chunks <- []byte("PBRABCQ")
	}()
	attempts, err := waitUntilReady(context.Background(), &writes, newStreamMatcher(chunks, done), "ABC", time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte("printf 'PBR")
	if runtime.GOOS == "windows" {
		needle = []byte("echo PBR")
	}
	if got := bytes.Count(writes.Bytes(), needle); got != 1 || attempts != 1 {
		t.Fatalf("readiness writes = %d attempts = %d", got, attempts)
	}
}

func TestRunProbeRetriesWithFreshMarkerAfterUncertainTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sub-10ms cancellation ordering is not deterministic on Windows")
	}
	chunks := make(chan []byte, 1)
	done := make(chan error, 1)
	var writes bytes.Buffer
	writeCount := 0
	writer := probeWriteFunc(func(data []byte) (int, error) {
		n, err := writes.Write(data)
		writeCount++
		if writeCount == 2 {
			// The first attempt has timed out before the retry is written.
			// Deliver its delayed output without assuming scheduler latency.
			chunks <- []byte("PBABC0000Q")
		}
		return n, err
	})
	latency, attempts, err := runProbe(context.Background(), writer, newStreamMatcher(chunks, done), "ABC", 0, 3, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || latency < 10*time.Millisecond || bytes.Count(writes.Bytes(), []byte("ABC0000")) != 2 {
		t.Fatalf("attempts=%d latency=%s writes=%q", attempts, latency, writes.String())
	}
}

type probeWriteFunc func([]byte) (int, error)

func (write probeWriteFunc) Write(data []byte) (int, error) { return write(data) }

func TestPercentileUsesNearestRank(t *testing.T) {
	values := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond}
	if got := percentile(values, 50); got != 3*time.Millisecond {
		t.Fatalf("p50 = %s", got)
	}
	if got := percentile(values, 99); got != 5*time.Millisecond {
		t.Fatalf("p99 = %s", got)
	}
}

func TestLabelsAreBoundedMetadata(t *testing.T) {
	for _, value := range []string{"quic", "tcp_dedicated", "download_and_upload", "rtt.20"} {
		if !validLabel(value) {
			t.Fatalf("valid label rejected: %q", value)
		}
	}
	for _, value := range []string{"", "Has Caps", "secret/path", "x@y"} {
		if validLabel(value) {
			t.Fatalf("invalid label accepted: %q", value)
		}
	}
}

func TestRunMeasuresEchoingSubprocess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := []string{"sh"}
	if runtime.GOOS == "windows" {
		command = []string{"cmd.exe", "/d", "/q", "/k"}
	}
	err := run(context.Background(), append([]string{"-samples=3", "-warmup=1", "-startup-delay=0", "-interval=0", "-probe-timeout=1s", "-mode=quic", "--"}, command...), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	var got result
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != "paperboat.terminal-benchmark/v1" || got.Mode != "quic" || got.Samples != 3 || got.ProbeAttempts != 3 || got.P99MS < got.P50MS {
		t.Fatalf("result = %+v", got)
	}
}

func TestRunStartsUnrecordedLoadBeforeProbes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	load := "printf load-started >&2"
	command := []string{"sh"}
	if runtime.GOOS == "windows" {
		load = "echo load-started 1>&2"
		command = []string{"cmd.exe", "/d", "/q", "/k"}
	}
	err := run(context.Background(), append([]string{"-samples=1", "-warmup=0", "-startup-delay=0", "-load-warmup=0", "-load-command=" + load, "-interval=0", "-probe-timeout=1s", "-mode=quic", "--"}, command...), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("load-started")) || bytes.Contains(stdout.Bytes(), []byte("printf")) {
		t.Fatalf("result exposed load command or output: %s", stdout.Bytes())
	}
	if bytes.Contains(stderr.Bytes(), []byte("load-started")) {
		t.Fatalf("benchmark exposed load output on stderr: %s", stderr.Bytes())
	}
}

func TestRunReportsSubprocessFailureAfterSuccessfulProbes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell exit-status fixture uses POSIX sh")
	}
	var stdout, stderr bytes.Buffer
	command := []string{"sh", "-c", `while IFS= read -r line; do eval "$line"; done; exit 17`}
	err := run(context.Background(), append([]string{"-samples=1", "-warmup=0", "-startup-delay=0", "-interval=0", "-probe-timeout=1s", "-mode=quic", "--"}, command...), &stdout, &stderr)
	if err == nil || err.Error() != "benchmark command exited unsuccessfully" {
		t.Fatalf("run error=%v stdout=%q", err, stdout.Bytes())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("subprocess exit cause was not preserved: %T %v", err, err)
	}
}

func TestRunRetainsExitAfterInterruptDuringNormalShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal fixture uses POSIX sh")
	}
	var stdout, stderr bytes.Buffer
	command := []string{"sh", "-c", `while IFS= read -r line; do eval "$line"; done; sleep 30 >/dev/null 2>&1 & child=$!; trap 'kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; exit 17' INT; wait "$child"`}
	err := run(context.Background(), append([]string{"-samples=1", "-warmup=0", "-startup-delay=0", "-interval=0", "-probe-timeout=1s", "-mode=quic", "--"}, command...), &stdout, &stderr)
	if err == nil || err.Error() != "benchmark command exited unsuccessfully" {
		t.Fatalf("run error=%v stdout=%q", err, stdout.Bytes())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("post-interrupt child exit was not preserved: %T %v", err, err)
	}
}

func TestRunAcceptsForcedProcessTerminationDuringNormalShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses POSIX sh")
	}
	var stdout, stderr bytes.Buffer
	command := []string{"sh", "-c", `count=0; while IFS= read -r line; do eval "$line"; count=$((count + 1)); if [ "$count" -ge 2 ]; then trap '' INT; exec sleep 30; fi; done`}
	err := run(context.Background(), append([]string{"-samples=1", "-warmup=0", "-startup-delay=0", "-ready-interval=5ms", "-ready-timeout=1s", "-interval=0", "-probe-timeout=1s", "--"}, command...), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	var got result
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("benchmark result: %v, output=%q", err, stdout.Bytes())
	}
	if got.Samples != 1 {
		t.Fatalf("benchmark result = %+v", got)
	}
}

func TestRunDoesNotEchoInvalidFlagValue(t *testing.T) {
	const privateValue = "PRIVATE_BENCHMARK_FLAG_VALUE"
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-samples=" + privateValue, "--", "sh"}, &stdout, &stderr)
	if err == nil || err.Error() != "invalid benchmark flags" || strings.Contains(err.Error(), privateValue) || strings.Contains(stderr.String(), privateValue) {
		t.Fatalf("invalid flag output exposed input: err=%v stderr=%q", err, stderr.String())
	}
}

func TestRunReapsSubprocessAfterParentCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess fixture uses POSIX sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := []string{"sh", "-c", `while IFS= read -r line; do :; done`}
	started := time.Now()
	err := run(ctx, append([]string{"-samples=1", "-warmup=0", "-startup-delay=0", "-ready-timeout=1s", "-ready-interval=5ms", "-interval=0", "-probe-timeout=1s", "--"}, command...), &stdout, &stderr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v, want parent deadline", err)
	}
	var cleanupFailure benchmarkFailure
	if errors.As(err, &cleanupFailure) {
		t.Fatalf("expected context-owned process termination to stay quiet, got %v (cause %T: %v)", err, cleanupFailure.cause, cleanupFailure.cause)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("subprocess cleanup exceeded bound: %s", elapsed)
	}
}
