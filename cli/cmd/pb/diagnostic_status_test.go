package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestDiagnosticConfigSyncPreservesMissingServiceAndUnknownAssignment(t *testing.T) {
	repo := "repository"
	enabled := api.ConfigAssignment{PullRepositoryID: &repo}
	disabled := api.ConfigAssignment{}
	for _, tc := range []struct {
		state      string
		assignment *api.ConfigAssignment
		err        error
		want       string
	}{
		{"not_installed", &disabled, nil, "not enabled"},
		{"not_installed", &enabled, nil, "service missing · configuration sync is assigned"},
		{"not_installed", nil, errors.New("ambiguous lookup"), "assignment not checked · service not installed"},
		{"invalid", &disabled, nil, "not enabled · service definition invalid"},
		{"unavailable", &enabled, nil, "service status unavailable · access or query failed"},
		{"installed_inactive", &enabled, nil, "installed · not running"},
	} {
		if got := diagnosticConfigSync(tc.state, tc.assignment, tc.err); got != tc.want {
			t.Errorf("state=%s got=%q want=%q", tc.state, got, tc.want)
		}
	}
}

func TestDiagnosticCredentialReadFailureIsNotLogout(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{config.ErrNoCredentials, "no saved sign-in found in this user profile"},
		{fmt.Errorf("read: %w", config.ErrCredentialStoreUnavailable), "saved sign-in unavailable · credential store access failed"},
		{config.ErrCredentialRequiresInteractiveLogin, "saved sign-in needs migration from an interactive session"},
		{errors.New("refresh failure"), "saved sign-in unavailable · credential read or refresh failed"},
	} {
		if got := diagnosticCredentialFailure(tc.err); got != tc.want {
			t.Errorf("got=%q want=%q", got, tc.want)
		}
	}
}

func TestDiagnosticWorkloadsNeverInventsZeroCounts(t *testing.T) {
	if got := diagnosticWorkloads(localDoctorReport{WorkloadCounts: "unavailable"}); got != "unavailable" {
		t.Fatalf("unavailable counts must not display zero workloads: %q", got)
	}
	if got := diagnosticWorkloads(localDoctorReport{WorkloadCounts: "available", TrackedSessions: 2, ActiveProcesses: 3, ActiveUploads: 1}); got != "2 sessions · 3 processes · 1 uploads" {
		t.Fatalf("actual counts not displayed: %q", got)
	}
}

func TestHomeRuntimeDiagnosticsDoesNotUseLivenessAsWorkloadCounts(t *testing.T) {
	for _, tc := range []struct{ name, diagnostics, want string }{
		{"absent counts", `{"schema":"paperboat.host-diagnostics/v1","health":{},"metrics":[],"events":[],"dropped_events":0}`, "unavailable · runtime did not provide workload counts"},
		{"actual counts", `{"schema":"paperboat.host-diagnostics/v1","health":{},"metrics":[],"events":[],"dropped_events":0,"workloads":{"sessions":2,"processes":3,"attachments":1,"uploads":4}}`, "available"},
		{"invalid counts", `{"schema":"paperboat.host-diagnostics/v1","health":{},"metrics":[],"events":[],"dropped_events":0,"workloads":{"sessions":-1}}`, "unavailable · invalid runtime diagnostics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/healthz" {
					w.Write([]byte(`{"live":true}`))
					return
				}
				w.Write([]byte(tc.diagnostics))
			}))
			defer server.Close()
			root := t.TempDir()
			os.Mkdir(filepath.Join(root, "runtime"), 0700)
			body, _ := json.Marshal(map[string]string{"schema": "paperboat.worker-local/v1", "listen_address": strings.TrimPrefix(server.URL, "http://")})
			os.WriteFile(filepath.Join(root, "runtime", "worker-local.json"), body, 0600)
			report := localDoctorReport{WorkloadCounts: "unavailable"}
			inspectLocalRuntimeHealth(&report, root)
			if report.HostRuntime != "ready" || report.WorkloadCounts != tc.want {
				t.Fatalf("runtime=%q counts=%q", report.HostRuntime, report.WorkloadCounts)
			}
			if tc.want == "available" && (report.TrackedSessions != 2 || report.ActiveUploads != 4) {
				t.Fatal("actual counts lost")
			}
		})
	}
}
