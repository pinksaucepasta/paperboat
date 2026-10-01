package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// This opt-in check exercises the installed user's real runtime and saved login.
// It is run on enrolled qualification machines, never with substitute services.
func TestInstalledHomeDiagnostics(t *testing.T) {
	if os.Getenv("PAPERBOAT_TEST_INSTALLED_DIAGNOSTICS") != "1" {
		t.Skip("requires an enrolled native installation")
	}
	report := collectLocalDoctor()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	deps, err := buildDeps(actionContext(cmd, nil))
	if err != nil {
		t.Fatal("could not inspect installed user configuration")
	}
	credential, err := deps.auth.Credential()
	if err != nil {
		t.Fatal(diagnosticCredentialFailure(err))
	}
	client := api.New(deps.cfg.ServerURL, credential, nil)
	if _, err := client.Me(ctx); err != nil {
		t.Fatal("saved login did not authenticate with control plane")
	}
	assignment, err := client.ConfigAssignment(ctx, report.MachineID)
	if err != nil {
		t.Fatal("configuration assignment could not be checked")
	}
	status := diagnosticConfigSync(report.ConfigService, &assignment, nil)
	summary := struct {
		Setup, Identity, Credential, ConfigSync, HostRuntime, Workloads string
		Authenticated                                                   bool
	}{
		report.SetupState, report.IdentityState, report.CredentialState, status, report.HostRuntime, diagnosticWorkloads(report), true,
	}
	body, _ := json.Marshal(summary)
	t.Log(string(body))
	if report.SetupState != "configured" || report.IdentityState != "valid" || report.CredentialState != "valid" {
		t.Error("installed machine identity/credential is not ready")
	}
	if report.HostRuntime != "ready" || report.WorkloadCounts != "available" {
		t.Error("installed runtime or actual workload counts are unavailable")
	}
	if configAssignmentEnabled(assignment) {
		if report.ConfigService != "active" {
			t.Error("assigned configuration worker is not running")
		}
	} else if status != "not enabled" {
		t.Error("unassigned installation has an unexplained configuration worker state")
	}
}
