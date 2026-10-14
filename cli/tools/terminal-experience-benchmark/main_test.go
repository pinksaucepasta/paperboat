//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type failedHelpWriter struct{}

func (failedHelpWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRunDoesNotEchoInvalidFlagValue(t *testing.T) {
	const canary = "PRIVATE_FLAG_CANARY"
	var output bytes.Buffer
	err := run(context.Background(), []string{"-ssh-port=" + canary}, &output)
	if err == nil {
		t.Fatal("run accepted an invalid SSH port")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(output.String(), canary) {
		t.Fatalf("invalid flag value escaped into output: error=%q output=%q", err, output.String())
	}
}

func TestRunReturnsHelpOutputFailure(t *testing.T) {
	err := run(context.Background(), []string{"-h"}, failedHelpWriter{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("help output failure was not retained: %v", err)
	}
}

func TestMeasurePreservesPrematureExit130(t *testing.T) {
	binDir := t.TempDir()
	ssh := filepath.Join(binDir, "ssh")
	script := "#!/bin/sh\nprintf 'first\\n'; sleep 0.02; printf 'second\\n'; sleep 0.05; exit 130\n"
	if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := measure(context.Background(), config{duration: time.Second}, "ssh", 1)
	if err == nil {
		t.Fatal("measure accepted a child that exited before the measurement ended")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("premature exit status was not retained: %v", err)
	}
}

func TestMeasureReportsPBSessionCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	pb := filepath.Join(dir, "pb")
	trace := filepath.Join(dir, "calls")
	t.Setenv("PB_BENCH_TRACE", trace)
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> \"$PB_BENCH_TRACE\"\nconfirmed=0\nfor arg in \"$@\"; do [ \"$arg\" = --confirm ] && confirmed=1; done\ncase \"$1:$2\" in\nconnect:*) printf 'PRIVATE_OUTPUT_CANARY\\n'; exit 0 ;;\nsession:close) if [ \"$confirmed\" -eq 1 ]; then printf '{\"state\":\"closed\",\"operation_state\":\"applied\"}\\n'; exit 0; fi; printf '{\"data\":{\"confirmation_token\":\"C12345\"}}\\n'; exit 2 ;;\nsession:delete) if [ \"$confirmed\" -eq 1 ]; then exit 23; fi; printf '{\"data\":{\"confirmation_token\":\"D12345\"}}\\n'; exit 2 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(pb, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		pbStartup: 5 * time.Second,
		pb:        pb,
		target:    "fixture",
	}
	_, err := measure(context.Background(), cfg, "pb_quic", 1)
	if err == nil {
		t.Fatal("measure returned success after session cleanup failed")
	}
	if !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanup failure was not reported safely: %q", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_OUTPUT_CANARY") || strings.Contains(err.Error(), pb) {
		t.Fatalf("error exposed terminal content or executable path: %q", err)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("confirmed original cleanup exit cause was not retained: %v", err)
	}
	calls, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := strings.TrimSpace(string(calls)), "connect fixture\nsession close\nsession close\nsession delete\nsession delete"; got != want {
		t.Fatalf("session cleanup did not close and confirm before deletion:\n got %q\nwant %q", got, want)
	}
}

func TestDeletePBSessionLeavesPendingCloseRecordIntact(t *testing.T) {
	dir := t.TempDir()
	pb := filepath.Join(dir, "pb")
	trace := filepath.Join(dir, "calls")
	t.Setenv("PB_BENCH_TRACE", trace)
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> \"$PB_BENCH_TRACE\"\ncase \"$1:$2\" in\nsession:close) printf '{\"state\":\"closing\",\"operation_state\":\"pending\"}\\n'; exit 0 ;;\nsession:delete) exit 0 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(pb, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	err := deletePBSession(pb, "fixture", "owned-session")
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending close result = %v, want actionable pending error", err)
	}
	calls, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := strings.TrimSpace(string(calls)), "session close"; got != want {
		t.Fatalf("pending close must not delete the session record: calls=%q", got)
	}
}

func TestSummarizeOutputCadence(t *testing.T) {
	base := time.Unix(0, 0)
	events := []readEvent{
		{at: base, n: 10},
		{at: base.Add(10 * time.Millisecond), n: 20},
		{at: base.Add(30 * time.Millisecond), n: 30},
		{at: base.Add(130 * time.Millisecond), n: 40},
	}
	got := summarize("quic", 2, time.Second, events)
	if got.Bytes != 100 || got.Events != 4 || got.BytesSec != 100 {
		t.Fatalf("volume summary = %+v", got)
	}
	if got.GapP50MS != 20 || got.GapP95MS != 100 || got.GapP99MS != 100 || got.MaxGapMS != 100 {
		t.Fatalf("gap summary = %+v", got)
	}
	if got.Over16MS != 2 || got.Over33MS != 1 || got.Over50MS != 1 || got.Over100MS != 0 {
		t.Fatalf("threshold summary = %+v", got)
	}
}

func TestSummarizeHandlesSparseOutput(t *testing.T) {
	got := summarize("ssh", 1, time.Second, []readEvent{{at: time.Now(), n: 7}})
	if got.Bytes != 7 || got.Events != 1 || got.MaxGapMS != 0 {
		t.Fatalf("summary = %+v", got)
	}
}

func TestPercentileUsesNearestRank(t *testing.T) {
	values := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond}
	if got := percentile(values, 50); got != 3*time.Millisecond {
		t.Fatalf("p50 = %v", got)
	}
	if got := percentile(values, 99); got != 5*time.Millisecond {
		t.Fatalf("p99 = %v", got)
	}
}

func TestSummarizeRuns(t *testing.T) {
	got := summarizeRuns("pb_quic", []sample{
		{BytesSec: 100, GapP95MS: 20, GapP99MS: 30, MaxGapMS: 80, Over33MS: 2, Over50MS: 1},
		{BytesSec: 140, GapP95MS: 22, GapP99MS: 35, MaxGapMS: 120, Over33MS: 3, Over50MS: 2, Over100MS: 1},
	})
	if got.Runs != 2 || got.MeanBytesSec != 120 || got.MedianP95MS != 20 || got.MedianP99MS != 30 || got.MedianMaxMS != 80 || got.WorstGapMS != 120 {
		t.Fatalf("aggregate = %+v", got)
	}
	if got.TotalOver33MS != 5 || got.TotalOver50MS != 3 || got.TotalOver100MS != 1 {
		t.Fatalf("aggregate thresholds = %+v", got)
	}
}
