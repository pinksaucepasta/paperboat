//go:build linux

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxSystemTrustDenialIsPending(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	trust := filepath.Join(directory, "trust")
	if err = os.Mkdir(trust, 0755); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "update-ca-certificates" {
			t.Fatalf("unexpected command %q", name)
		}
		return []byte("update-ca-certificates: permission denied"), errors.New("exit status 1")
	}
	err = installLinuxSystemTrustAt(t.Context(), trust, filepath.Join(directory, "bundle.pem"), root, rootPEM, uint32(os.Geteuid()), runner)
	if !errors.Is(err, ErrTrustPending) {
		t.Fatalf("system trust denial is not pending: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("pending trust hides the denial cause: %v", err)
	}
}
