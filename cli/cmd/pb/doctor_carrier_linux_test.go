//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// Lane H's round-33 finding: after an unsupported downgrade the carrier unit restarted every few seconds,
// every ping and exec failed, and `pb doctor` said "healthy". The values below were read from that unit.
func TestDoctorWarnsWhenTheNativeCarrierKeepsFailingToStart(t *testing.T) {
	systemd := t.TempDir()
	const unit = "paperboat-carrier-u1000.service"
	check := func(properties map[string]string, err error) doctorpkg.Check {
		return doctorCarrierCheck(t.Context(), systemd, unit, func(_ context.Context, asked string) (map[string]string, error) {
			if asked != unit {
				t.Fatalf("asked about %q, want %q", asked, unit)
			}
			return properties, err
		})
	}

	loop := check(map[string]string{"LoadState": "loaded", "ActiveState": "activating", "SubState": "auto-restart", "Result": "exit-code", "NRestarts": "49"}, nil)
	if loop.Status != doctorpkg.StatusWarning || !strings.Contains(loop.Summary, "keeps failing to start") || !strings.Contains(loop.Summary, "restarted 49 times") || !strings.Contains(loop.Recovery, "journalctl -u "+unit) || !strings.Contains(loop.Recovery, "older Paperboat over a newer one") {
		t.Fatalf("a restart loop was not reported: %+v", loop)
	}
	failed := check(map[string]string{"LoadState": "loaded", "ActiveState": "failed", "SubState": "failed", "Result": "exit-code"}, nil)
	if failed.Status != doctorpkg.StatusWarning {
		t.Fatalf("a failed unit reported %+v", failed)
	}
	// Between two tries of a loop the unit is briefly "starting", but its last result is a failure.
	between := check(map[string]string{"LoadState": "loaded", "ActiveState": "activating", "SubState": "start", "Result": "exit-code", "NRestarts": "3"}, nil)
	if between.Status != doctorpkg.StatusWarning {
		t.Fatalf("a start after a failed result reported %+v", between)
	}

	if healthy := check(map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "Result": "success"}, nil); healthy.Status != doctorpkg.StatusPass {
		t.Fatalf("a running carrier reported %+v", healthy)
	}
	starting := check(map[string]string{"LoadState": "loaded", "ActiveState": "activating", "SubState": "start", "Result": "success", "NRestarts": "0"}, nil)
	if starting.Status != doctorpkg.StatusPending {
		t.Fatalf("an ordinary start reported %+v", starting)
	}
	if absent := check(map[string]string{"LoadState": "not-found", "ActiveState": "inactive"}, nil); absent.Status != doctorpkg.StatusPass {
		t.Fatalf("a computer without a carrier unit reported %+v", absent)
	}
	if unknown := check(nil, errors.New("systemctl failed")); unknown.Status != doctorpkg.StatusUnavailable {
		t.Fatalf("an unanswered question reported %+v", unknown)
	}
	if err := os.Remove(systemd); err != nil {
		t.Fatal(err)
	}
	if other := check(nil, errors.New("not asked")); other.Status != doctorpkg.StatusPass {
		t.Fatalf("a host without systemd reported %+v", other)
	}
}
