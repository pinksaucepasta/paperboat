package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
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
	// An unrelated credential failure keeps the generic guidance.
	other := doctorCredentialFailure(base, errors.New("keyring unavailable"))
	if strings.Contains(other.Summary, "removed") || strings.Contains(other.Recovery, "pb login") {
		t.Fatalf("a storage failure was reported as a removed computer: %+v", other)
	}
}
