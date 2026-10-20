package main

import (
	"context"
	"errors"
	"fmt"
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
