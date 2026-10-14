//go:build darwin

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinTrustApprovalDenialIsPending(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	pemPath := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(pemPath, rootPEM, 0644); err != nil {
		t.Fatal(err)
	}
	const denial = "The authorization was denied since no user interaction was possible."
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
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
	err := ensureDarwinSystemTrust(t.Context(), pemPath, runner)
	if !errors.Is(err, ErrTrustPending) {
		t.Fatalf("keychain approval denial is not pending: %v", err)
	}
	if !strings.Contains(err.Error(), denial) {
		t.Fatalf("pending trust hides the approval denial: %v", err)
	}
}
