package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestConfigSyncMenuFollowsAuthoritativeAssignment(t *testing.T) {
	repo := "repo"
	for _, tc := range []struct {
		name          string
		assignment    api.ConfigAssignment
		wantConfigure string
		disable       bool
	}{
		{"disabled", api.ConfigAssignment{}, "Enable sync", false},
		{"pull", api.ConfigAssignment{PullRepositoryID: &repo, Mode: "pull_only"}, "Configure sync", true},
		{"push", api.ConfigAssignment{PushRepositoryID: &repo, Mode: "push_only"}, "Configure sync", true},
		{"legacy repository field", api.ConfigAssignment{RepositoryID: &repo, Mode: "bidirectional"}, "Configure sync", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := configSyncMenuItems(tc.assignment)
			found := map[string]string{}
			for _, item := range items {
				found[item.ID] = item.Title
			}
			if found["configure"] != tc.wantConfigure || (found["disable"] != "") != tc.disable {
				t.Fatalf("menu does not match assignment: %+v", items)
			}
			if found["status"] == "" || found["repositories"] == "" {
				t.Fatal("status or repository onboarding unavailable")
			}
		})
	}
}

func TestGitHubConnectionPollingTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		state, id string
		success   bool
	}{
		{"completed", "link", true}, {"failed", "link", false}, {"expired", "link", false}, {"canceled", "link", false}, {"unknown", "link", false}, {"completed", "other", false},
	} {
		t.Run(tc.state+tc.id, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"data": api.GitHubNativeLink{ID: tc.id, State: tc.state}})
			}))
			defer server.Close()
			client := api.New(server.URL, config.Credential{AccessToken: "native"}, nil)
			err := waitForGitHubNativeLink(context.Background(), client, api.GitHubNativeLink{ID: "link"})
			if (err == nil) != tc.success {
				t.Fatalf("terminal state result=%v", err)
			}
		})
	}
}

func TestGitHubConnectionPollingCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": api.GitHubNativeLink{ID: "link", State: "pending"}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := waitForGitHubNativeLink(ctx, api.New(server.URL, config.Credential{AccessToken: "native"}, nil), api.GitHubNativeLink{ID: "link", PollIntervalSeconds: 2})
	if err == nil {
		t.Fatal("pending authorization ignored cancellation")
	}
}

func TestConfigSyncEnrollmentOnlyTreatsPureMissingRegistrationAsNotEnrolled(t *testing.T) {
	missing := &os.PathError{Op: "lstat", Path: "/private/identity/machine-registration.json", Err: os.ErrNotExist}
	if err := configSyncEnrollmentError("", missing); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("missing registration error=%v, want setup guidance", err)
	}

	operationalCause := errors.New("operation failed")
	joined := &os.PathError{Op: "lstat", Path: "/private/identity/machine-registration.json", Err: errors.Join(os.ErrNotExist, operationalCause)}
	err := configSyncEnrollmentError("", joined)
	if err == nil || strings.Contains(err.Error(), "not enrolled") || !errors.Is(err, operationalCause) {
		t.Fatalf("mixed registration failure was misclassified or lost: %v", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || strings.Contains(err.Error(), "/private/identity") {
		t.Fatalf("registration cause was not retained privately: %T %q", err, err.Error())
	}
	if err := configSyncEnrollmentError("machine_1", nil); err != nil {
		t.Fatalf("valid registration rejected: %v", err)
	}
}

type cyclicNotExistCause struct{}

func (cyclicNotExistCause) Error() string       { return "cyclic cause" }
func (cause cyclicNotExistCause) Unwrap() error { return cause }

type nilNotExistCause struct{}

func (*nilNotExistCause) Error() string { return "nil cause" }

func TestOnlyNotExistCauseRejectsMalformedChains(t *testing.T) {
	if onlyNotExistCause(errors.Join(os.ErrNotExist, errors.New("operation failed"))) {
		t.Fatal("mixed missing-file and operational causes were treated as absence")
	}
	if onlyNotExistCause(cyclicNotExistCause{}) {
		t.Fatal("cyclic error chain was treated as absence")
	}
	var typedNil *nilNotExistCause
	if onlyNotExistCause(typedNil) {
		t.Fatal("typed-nil error was treated as absence")
	}
}

func TestConfigSyncSourceInspectionRetainsCauseAndDiagnosticPhase(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing-config.json")
	if exists, err := configSyncSourceExists(missingPath); exists || err != nil {
		t.Fatalf("missing source exists=%t err=%v", exists, err)
	}

	if _, err := configSyncSourceExists("\x00"); err == nil {
		t.Fatal("invalid source path was ignored")
	} else {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("source path cause lost: %T", err)
		}
		if strings.ContainsRune(err.Error(), '\x00') || !strings.Contains(err.Error(), "could not inspect") {
			t.Fatalf("unsafe source error: %q", err.Error())
		}
		stage, ok := err.(interface{ DiagnosticStage() string })
		if !ok || stage.DiagnosticStage() != "reconciliation" {
			t.Fatalf("source diagnostic stage=%v", err)
		}
		code, ok := err.(interface{ DiagnosticCode() string })
		if !ok || code.DiagnosticCode() != "config_sync_failed" {
			t.Fatalf("source diagnostic code=%v", err)
		}
	}
}

func TestGitHubLinkCleanupKeepsReferenceAndSurfacesMixedCancellationFailure(t *testing.T) {
	reference := "support_550e8400-e29b-41d4-a716-446655440000"
	parent, cancel := context.WithCancel(supportref.WithContext(context.Background(), reference))
	cleanup, stop := githubLinkCleanupContext(parent)
	defer stop()
	if got := supportref.FromContext(cleanup); got != reference || !supportref.Valid(got) {
		t.Fatalf("cleanup lost support reference: %q", got)
	}
	if _, ok := cleanup.Deadline(); !ok {
		t.Fatal("cleanup context has no bounded deadline")
	}
	cancel()
	if cleanup.Err() != nil {
		t.Fatalf("parent cancellation canceled cleanup context: %v", cleanup.Err())
	}

	privateCause := errors.New("provider response secret-marker")
	joined := joinGitHubLinkCancelFailure(selector.ErrCanceled, privateCause)
	if !errors.Is(joined, privateCause) || interactiveCanceled(joined) {
		t.Fatalf("mixed cleanup error lost its cause or was hidden as cancellation: %v", joined)
	}
	if strings.Contains(joined.Error(), privateCause.Error()) {
		t.Fatalf("cleanup error disclosed provider response: %q", joined.Error())
	}
}
