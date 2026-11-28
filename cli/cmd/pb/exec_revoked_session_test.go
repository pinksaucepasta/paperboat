package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

func stubExecListMachines(t *testing.T, list func(context.Context) error) {
	t.Helper()
	previous := execListMachines
	t.Cleanup(func() { execListMachines = previous })
	execListMachines = func(ctx context.Context, _ *api.Client, _ string) error { return list(ctx) }
}

// F2-05: when the local service could not be readied, a server refusal (401) is what exec reports.
func TestExecSessionRefusedReportsOnlyServerRefusal(t *testing.T) {
	stubExecListMachines(t, func(context.Context) error {
		return &api.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated"}
	})
	refused := execSessionRefused(context.Background(), nil, "peer")
	if refused == nil || !errors.Is(refused, api.ErrUnauthenticated) {
		t.Fatalf("a 401 must be reported, got %v", refused)
	}
	if got := safeExecError(refused); !strings.Contains(got, "no longer valid") || !strings.Contains(got, "pb login") {
		t.Fatalf("--json detail for a refused session = %q", got)
	}
	if got := commandFailureMessage(refused, classifyCommandFailure(refused)); !strings.Contains(got, "no longer valid") || !strings.Contains(got, "pb login") {
		t.Fatalf("message for a refused session = %q", got)
	}

	for _, other := range []error{nil, errors.New("network down"), &api.APIError{Status: http.StatusInternalServerError, Code: "internal"}} {
		stubExecListMachines(t, func(context.Context) error { return other })
		if err := execSessionRefused(context.Background(), nil, "peer"); err != nil {
			t.Fatalf("%v must leave the local error in place, got %v", other, err)
		}
	}
}

// The check is bounded, and it stops with the command (Ctrl-C) instead of outliving it.
func TestExecSessionRefusedIsBoundedAndFollowsCommandCancel(t *testing.T) {
	previousTimeout := execSessionRefusedTimeout
	t.Cleanup(func() { execSessionRefusedTimeout = previousTimeout })
	execSessionRefusedTimeout = 50 * time.Millisecond
	stubExecListMachines(t, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	started := time.Now()
	if err := execSessionRefused(context.Background(), nil, "peer"); err != nil {
		t.Fatalf("a check that times out must not be reported: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("check must end at its bound, took %s", elapsed)
	}

	execSessionRefusedTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	started = time.Now()
	if err := execSessionRefused(ctx, nil, "peer"); err != nil {
		t.Fatalf("a canceled check must not be reported: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("check must stop with the command, took %s", elapsed)
	}
}

// A refused recipe or API token is not a session refusal: its own message names the recovery, not `pb login`.
func TestExecSessionRefusalExcludesRecipeAndAPITokens(t *testing.T) {
	for _, code := range []string{"recipe_expired", "recipe_replaced", "recipe_invalid", "api_token_invalid"} {
		err := &api.APIError{Status: http.StatusUnauthorized, Code: code}
		if execSessionRefusal(err) {
			t.Fatalf("%s must not read as a session refusal", code)
		}
		if strings.Contains(safeExecError(err), "pb login") {
			t.Fatalf("%s --json detail must not say pb login: %q", code, safeExecError(err))
		}
	}
	if !execSessionRefusal(api.ErrUnauthenticated) || !execSessionRefusal(&api.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated"}) {
		t.Fatal("a plain 401 is a session refusal")
	}
}

// A list that ignores its context (a credential refresh runs without one) still cannot hold exec past the cap.
func TestExecSessionRefusedBoundsAListThatIgnoresItsContext(t *testing.T) {
	previousTimeout := execSessionRefusedTimeout
	t.Cleanup(func() { execSessionRefusedTimeout = previousTimeout })
	execSessionRefusedTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubExecListMachines(t, func(context.Context) error {
		<-release
		return &api.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated"}
	})
	started := time.Now()
	if err := execSessionRefused(context.Background(), nil, "peer"); err != nil {
		t.Fatalf("a check past its cap must not be reported: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("check must end at its cap, took %s", elapsed)
	}
}
