package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

// F2-05/F2-07: a computer removed from the account loses its sign-in; doctor says so and gives the
// one step, instead of "check local credential storage".
func TestDoctorNamesARemovedComputerAndTheStepToAddItAgain(t *testing.T) {
	base := doctorpkg.Check{Category: "control", Code: "authentication", Status: doctorpkg.StatusFail, Summary: "Paperboat authentication is unavailable.", Recovery: "Check local credential storage and the Paperboat service, then run `pb doctor` again."}
	rejected := fmt.Errorf("refresh Paperboat session: %w", &api.APIError{Status: 400, Code: "invalid_grant"})
	check := doctorCredentialFailure(base, &sessionauth.SignedOutError{Cause: rejected})
	if !strings.Contains(check.Summary, "removed from the account") || !strings.Contains(check.Recovery, "pb login") || check.Status != doctorpkg.StatusFail {
		t.Fatalf("check=%+v", check)
	}
	// A refresh the server rejects (the removed computer's credential) reads the same way.
	refused := fmt.Errorf("account credentials: %w", errors.Join(fmt.Errorf("refresh Paperboat session"), &api.APIError{Status: 401, Code: "unauthenticated"}))
	if got := doctorCredentialFailure(base, refused); !strings.Contains(got.Summary, "removed from the account") || !strings.Contains(got.Recovery, "pb login") {
		t.Fatalf("a rejected refresh was not explained as a removed computer: %+v", got)
	}
	// An unrelated credential failure keeps the generic guidance.
	other := doctorCredentialFailure(base, errors.New("keyring unavailable"))
	if strings.Contains(other.Summary, "removed") || strings.Contains(other.Recovery, "pb login") {
		t.Fatalf("a storage failure was reported as a removed computer: %+v", other)
	}
}

func TestDoctorSendsAComputerAwaitingEnrollmentToPbLogin(t *testing.T) {
	snapshot := localapi.Snapshot{DaemonState: "awaiting_enrollment"}
	for _, probe := range doctorProbes(&cobra.Command{}, snapshot, nil) {
		if probe.Code != "daemon" {
			continue
		}
		check := probe.Run(context.Background())
		if !strings.Contains(check.Recovery, "pb login") || strings.Contains(check.Recovery, "Restart") {
			t.Fatalf("awaiting enrollment was sent to a restart: %+v", check)
		}
		return
	}
	t.Fatal("no daemon probe")
}

// C020: right after `pb setup` the daemon is still starting; doctor says so (pending, overall
// "starting") and does not send the user to restart a service that is only starting.
func TestDoctorReportsAStartingDaemonAsPendingNotAFault(t *testing.T) {
	snapshot := localapi.Snapshot{DaemonState: "starting"}
	for _, probe := range doctorProbes(&cobra.Command{}, snapshot, nil) {
		if probe.Code != "daemon" {
			continue
		}
		check := probe.Run(context.Background())
		if check.Status != doctorpkg.StatusPending || strings.Contains(check.Recovery, "Restart") || !strings.Contains(check.Recovery, "pb doctor") {
			t.Fatalf("a starting daemon was reported as a fault: %+v", check)
		}
		return
	}
	t.Fatal("no daemon probe")
}

// F14-07: a server that refuses an out-of-date client is answered with `pb update`, not with advice
// to sign in again.
func TestDoctorNamesAnUnsupportedVersionInsteadOfAFailedLogin(t *testing.T) {
	for name, err := range map[string]error{
		"update_required": &api.APIError{Status: http.StatusUpgradeRequired, Code: "update_required"},
		"wrapped":         fmt.Errorf("health check: %w", &api.APIError{Status: http.StatusUpgradeRequired, Code: "update_required"}),
		"incompatible":    &api.ErrIncompatibleVersion{Required: "2"},
	} {
		if doctorUnsupportedVersion(err) == nil {
			t.Fatalf("%s: not recognized as an unsupported version", name)
		}
	}
	for name, err := range map[string]error{
		"unauthorized": &api.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated"},
		"timeout":      context.DeadlineExceeded,
	} {
		if doctorUnsupportedVersion(err) != nil {
			t.Fatalf("%s: wrongly treated as an unsupported version", name)
		}
	}
}
