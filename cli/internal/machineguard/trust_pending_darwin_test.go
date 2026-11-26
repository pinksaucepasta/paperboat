//go:build darwin

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinTrustApprovalDenialIsPending(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	pemPath := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(pemPath, rootPEM, 0644); err != nil {
		t.Fatal(err)
	}
	const denial = "The authorization was denied since no user interaction was possible."
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "/bin/launchctl" && args[0] == "asuser" { // the add runs in the console user session
			name, args = args[2], args[3:]
		}
		if name != "/usr/bin/security" {
			t.Fatalf("unexpected command %q", name)
		}
		if args[0] == "verify-cert" {
			return []byte("certificate untrusted"), errors.New("exit status 1")
		}
		if args[0] != "add-trusted-cert" {
			t.Fatalf("unexpected security operation %q", args[0])
		}
		return []byte(denial), errors.New("exit status 1")
	}
	err := ensureDarwinSystemTrust(t.Context(), pemPath, 501, runner)
	if !errors.Is(err, ErrTrustPending) {
		t.Fatalf("keychain approval denial is not pending: %v", err)
	}
	if !strings.Contains(err.Error(), denial) {
		t.Fatalf("pending trust hides the approval denial: %v", err)
	}
}

// A remote Mac: the add-trusted-cert prompt waits for a click nobody makes. The bound ends it as pending.
func TestDarwinTrustUnansweredPromptIsPendingWithinBound(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	pemPath := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(pemPath, rootPEM, 0644); err != nil {
		t.Fatal(err)
	}
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/security" && args[0] == "verify-cert" {
			return nil, errors.New("exit status 1")
		}
		<-ctx.Done() // the prompt is never answered
		return nil, errors.New("signal: killed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := ensureDarwinSystemTrust(ctx, pemPath, 501, runner)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("trust step outlived its bound: %v", time.Since(start))
	}
	if !errors.Is(err, ErrTrustPending) || !strings.Contains(err.Error(), "retry `pb install`") {
		t.Fatalf("unanswered prompt is not pending with recovery: %v", err)
	}
}
