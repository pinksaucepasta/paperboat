package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/spf13/cobra"
)

func freshResetTokenFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + "token"
	if err := os.WriteFile(path, []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFreshEnrollmentResetRequiresCleanupBeforeSuccess(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	original := freshEnrollmentCleanup
	t.Cleanup(func() { freshEnrollmentCleanup = original })
	cleanupCalls := 0
	freshEnrollmentCleanup = func(*cobra.Command) error {
		cleanupCalls++
		return errors.New("retained service")
	}
	command := freshEnrollmentResetCommand()
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", freshResetTokenFile(t), "--json"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "retained service") {
		t.Fatalf("reset error = %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d", cleanupCalls)
	}
}

func TestFreshEnrollmentResetReportsOnlyAfterCleanup(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	original := freshEnrollmentCleanup
	t.Cleanup(func() { freshEnrollmentCleanup = original })
	freshEnrollmentCleanup = func(*cobra.Command) error { return nil }
	var output bytes.Buffer
	command := freshEnrollmentResetCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", freshResetTokenFile(t), "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ok":true`, `"reset":true`, `"inbox_preserved":true`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q missing %q", output.String(), want)
		}
	}
}

func TestFreshEnrollmentCleanupRetainsStateAfterServiceFailure(t *testing.T) {
	statePurged := false
	runtimeAttempted := false
	err := performFreshEnrollmentCleanup(nil,
		func() error { return errors.New("daemon still running") },
		func() error { return nil },
		func() error { return nil },
		func() error { runtimeAttempted = true; return nil },
		func() error { statePurged = true; return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "stop local daemon") || !strings.Contains(err.Error(), "remove user Paperboat state") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !runtimeAttempted {
		t.Fatal("independent runtime teardown was not attempted")
	}
	if statePurged {
		t.Fatal("credentials/state were deleted after unresolved daemon failure")
	}
}

func TestFreshEnrollmentResetPreservesMatchingResume(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	token := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	record := bootstrap.NewResumeRecord("https://api.example.test", "public-key", token, "device", "host", "verifier-012345678901234567890123456789", time.Now().Add(time.Hour))
	if err := bootstrap.SaveResume(root, record); err != nil {
		t.Fatal(err)
	}
	tokenFile := freshResetTokenFile(t)
	original := freshEnrollmentCleanup
	originalExecutable := resolveFreshEnrollmentExecutable
	t.Cleanup(func() {
		freshEnrollmentCleanup = original
		resolveFreshEnrollmentExecutable = originalExecutable
	})
	freshEnrollmentCleanup = func(*cobra.Command) error { t.Fatal("matching resume was reset"); return nil }
	resolveFreshEnrollmentExecutable = func() (string, error) { return "/owned/pb", nil }
	var output bytes.Buffer
	command := freshEnrollmentResetCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", tokenFile, "--state-root", root, "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"resume":true`, `"reset":false`, `"executable":"/owned/pb"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q missing %q", output.String(), want)
		}
	}
}
