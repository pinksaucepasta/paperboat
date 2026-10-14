//go:build darwin || linux

package daemoncmd

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateProbeUpdaterOwnerNamespace(t *testing.T) {
	for _, tc := range []struct {
		platform string
		uid      int
		socket   string
	}{
		{"darwin", 501, "/var/run/paperboat-updated-u501/control.sock"},
		{"linux", 1000, "/run/paperboat-updated-u1000/control.sock"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			version, err := updateProbeUpdaterVersion(context.Background(), tc.platform, tc.uid, func(_ context.Context, socket string) (string, error) {
				if socket != tc.socket {
					t.Fatalf("probe contacted %q, want enrolled owner %q", socket, tc.socket)
				}
				return "2026.09.30.11", nil
			})
			if err != nil || version != "2026.09.30.11" {
				t.Fatalf("version=%q err=%v", version, err)
			}
		})
	}
}

func TestUpdateProbeUpdaterFailuresPropagate(t *testing.T) {
	failure := errors.New("updater unavailable")
	if version, err := updateProbeUpdaterVersion(context.Background(), "darwin", 501, func(context.Context, string) (string, error) {
		return "", failure
	}); version != "" || !errors.Is(err, failure) {
		t.Fatalf("missing updater was concealed: version=%q err=%v", version, err)
	}
	if _, err := updateProbeUpdaterVersion(context.Background(), "darwin", -1, func(context.Context, string) (string, error) {
		t.Fatal("invalid owner contacted updater")
		return "", nil
	}); err == nil {
		t.Fatal("invalid owner accepted")
	}
}
