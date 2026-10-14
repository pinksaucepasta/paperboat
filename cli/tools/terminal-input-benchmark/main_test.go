//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSummaries(t *testing.T) {
	values := []time.Duration{100 * time.Millisecond, 150 * time.Millisecond, 250 * time.Millisecond, 600 * time.Millisecond}
	value := summarize("pb_quic", 1, values)
	if value.P50MS != 150 || value.P95MS != 600 || value.MaxMS != 600 || value.Over200MS != 2 || value.Over500MS != 1 {
		t.Fatalf("result = %+v", value)
	}
	aggregate := summarizeRuns("pb_quic", []result{value, result{Samples: 4, P50MS: 120, P95MS: 300, P99MS: 400, MaxMS: 450, Over200MS: 1}})
	if aggregate.Samples != 8 || aggregate.MedianP50MS != 120 || aggregate.MedianP95MS != 300 || aggregate.MedianMaxMS != 450 || aggregate.WorstMS != 600 || aggregate.TotalOver200MS != 3 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
}

func TestMedian(t *testing.T) {
	if got := median([]float64{3, 1, 2}); got != 2 {
		t.Fatalf("median = %v", got)
	}
}

func TestMeasureReadinessErrorDoesNotExposePTYOutput(t *testing.T) {
	const privateOutput = "PRIVATE_TERMINAL_BANNER"
	binDir := t.TempDir()
	ssh := filepath.Join(binDir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' '" + privateOutput + "'\nexit 23\n"
	if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := measure(context.Background(), config{samples: 1, startup: 50 * time.Millisecond, timeout: 100 * time.Millisecond}, "ssh", 1)
	if err == nil {
		t.Fatal("measure unexpectedly accepted a process that never answered")
	}
	if strings.Contains(err.Error(), privateOutput) {
		t.Fatalf("measurement error exposed terminal output: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("measurement lost the terminal process failure: %v", err)
	}
}

func TestMeasureReadinessTimeoutJoinsLocalProcessCleanup(t *testing.T) {
	const privateOutput = "PRIVATE_TERMINAL_BANNER"
	binDir := t.TempDir()
	ssh := filepath.Join(binDir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' '" + privateOutput + "'\nexec cat >/dev/null\n"
	if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	started := time.Now()
	_, err := measure(context.Background(), config{samples: 1, startup: 50 * time.Millisecond, timeout: 100 * time.Millisecond}, "ssh", 1)
	if err == nil || err.Error() != "raw responder did not become ready" || !errors.Is(err, errResponseTimeout) {
		t.Fatalf("readiness failure was not preserved safely: %v", err)
	}
	if strings.Contains(err.Error(), privateOutput) {
		t.Fatalf("measurement error exposed terminal output: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled local process cleanup exceeded bound: %s", elapsed)
	}
}

func TestDeletePBSessionReportsFailedConfirmationSafely(t *testing.T) {
	const privateToken = "PRIVATE_SESSION_CONFIRMATION_TOKEN"
	binDir := t.TempDir()
	pb := filepath.Join(binDir, "pb")
	trace := filepath.Join(binDir, "calls")
	t.Setenv("PB_BENCH_TRACE", trace)
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> \"$PB_BENCH_TRACE\"\nconfirmed=0\nfor arg in \"$@\"; do [ \"$arg\" = --confirm ] && confirmed=1; done\ncase \"$1:$2\" in\nsession:close) if [ \"$confirmed\" -eq 1 ]; then printf '{\"state\":\"closed\",\"operation_state\":\"applied\"}\\n'; exit 0; fi; printf '{\"data\":{\"confirmation_token\":\"C12345\"}}\\n'; exit 2 ;;\nsession:delete) if [ \"$confirmed\" -eq 1 ]; then exit 19; fi; printf '{\"data\":{\"confirmation_token\":\"" + privateToken + "\"}}\\n'; exit 2 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(pb, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	err := deletePBSession(pb, "PRIVATE_TARGET", "PRIVATE_SESSION")
	if err == nil || err.Error() != "temporary session cleanup failed" || strings.Contains(err.Error(), privateToken) || strings.Contains(err.Error(), "PRIVATE_TARGET") || strings.Contains(err.Error(), "PRIVATE_SESSION") {
		t.Fatalf("session cleanup exposed inputs or missed failure: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 19 {
		t.Fatalf("session cleanup lost the command failure: %v", err)
	}
	calls, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := strings.TrimSpace(string(calls)), "session close\nsession close\nsession delete\nsession delete"; got != want {
		t.Fatalf("session cleanup did not close before delete:\n got %q\nwant %q", got, want)
	}
}

func TestDeletePBSessionLeavesPendingCloseRecordIntact(t *testing.T) {
	binDir := t.TempDir()
	pb := filepath.Join(binDir, "pb")
	trace := filepath.Join(binDir, "calls")
	t.Setenv("PB_BENCH_TRACE", trace)
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> \"$PB_BENCH_TRACE\"\ncase \"$1:$2\" in\nsession:close) printf '{\"state\":\"closing\",\"operation_state\":\"pending\"}\\n'; exit 0 ;;\nsession:delete) exit 0 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(pb, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	err := deletePBSession(pb, "PRIVATE_TARGET", "PRIVATE_SESSION")
	if err == nil || err.Error() != "temporary session close is pending; record left intact" {
		t.Fatalf("pending close result = %v", err)
	}
	calls, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.TrimSpace(string(calls)); got != "session close" {
		t.Fatalf("pending close must not delete the session record: calls=%q", got)
	}
}

func TestRunCancellationReapsProcessAndCleansTemporarySession(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		prompt string
	}{
		{name: "waiting for shell prompt"},
		{name: "waiting for responder", prompt: "$ "},
	} {
		scenario := scenario
		t.Run(scenario.name, func(t *testing.T) {
			binDir := t.TempDir()
			ssh := filepath.Join(binDir, "ssh")
			sshScript := "#!/bin/bash\nstty raw -echo\nwhile IFS= read -r -n 1 key; do\n  if [[ \"$key\" == $'\\003' ]]; then exit 0; fi\n  printf 'PBIN%s' \"$key\"\ndone\n"
			if err := os.WriteFile(ssh, []byte(sshScript), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			trace := filepath.Join(binDir, "calls")
			pidFile := filepath.Join(binDir, "pid")
			pb := filepath.Join(binDir, "pb")
			t.Setenv("PB_BENCH_TRACE", trace)
			t.Setenv("PB_BENCH_PID", pidFile)
			t.Setenv("PB_BENCH_PROMPT", scenario.prompt)
			pbScript := "#!/bin/sh\ncase \"$1:$2\" in\nconnect:*) printf 'connect\\n' >> \"$PB_BENCH_TRACE\"; printf '%s\\n' \"$$\" > \"$PB_BENCH_PID\"; printf '%s' \"$PB_BENCH_PROMPT\"; exec sleep 30 ;;\nsession:close) printf 'session close\\n' >> \"$PB_BENCH_TRACE\"; printf '{\"state\":\"closed\",\"operation_state\":\"applied\"}\\n' ;;\nsession:delete) printf 'session delete\\n' >> \"$PB_BENCH_TRACE\" ;;\nesac\nexit 0\n"
			if err := os.WriteFile(pb, []byte(pbScript), 0o700); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output bytes.Buffer
			args := []string{"-runs=1", "-samples=5", "-pb-startup=1s", "-timeout=1s", "-interval=0s", "-pb=" + pb, "-target=offline", "-ssh-host=offline"}
			finished := make(chan error, 1)
			reaped := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				select {
				case <-reaped:
				case <-time.After(3 * time.Second):
					t.Error("benchmark fixture did not finish after test cleanup")
				}
			})
			started := time.Now()
			go func() {
				defer close(reaped)
				finished <- run(ctx, args, &output)
			}()

			deadline := time.Now().Add(3 * time.Second)
			var pid int
			for time.Now().Before(deadline) {
				data, err := os.ReadFile(pidFile)
				if err == nil {
					pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil || pid <= 0 {
						t.Fatalf("invalid fixture process id: %q", strings.TrimSpace(string(data)))
					}
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("Paperboat fixture process did not start")
			}
			cancel()

			var runErr error
			select {
			case runErr = <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled benchmark did not finish cleanup")
			}
			if !errors.Is(runErr, context.Canceled) {
				t.Fatalf("benchmark lost the cancellation cause: %v", runErr)
			}
			if runErr.Error() != "pb_quic run 1 failed" {
				t.Fatalf("cancellation or cleanup was reported incorrectly: %v", runErr)
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("cancelled run did not preserve only the completed measurement: %q", output.String())
			}
			var completed result
			if err := json.Unmarshal([]byte(lines[0]), &completed); err != nil || completed.Transport != "ssh" {
				t.Fatalf("cancelled run changed completed output: result=%+v err=%v", completed, err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("process cancellation and session cleanup exceeded bound: %s", elapsed)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("cancelled fixture process was not reaped: %v", err)
			}
			calls, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := strings.TrimSpace(string(calls)), "connect\nsession close\nsession delete"; got != want {
				t.Fatalf("cancellation did not close then delete the session: got %q, want %q", got, want)
			}
		})
	}
}

func TestRunFlagFailureDoesNotEchoSuppliedValue(t *testing.T) {
	const privateValue = "PRIVATE_SSH_OPTION_VALUE"
	var output bytes.Buffer
	err := run(context.Background(), []string{"-ssh-port=" + privateValue}, &output)
	if err == nil || err.Error() != "invalid benchmark flags" || strings.Contains(err.Error(), privateValue) {
		t.Fatalf("flag failure exposed the supplied value: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid flags emitted benchmark output: %q", output.String())
	}
}
