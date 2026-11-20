package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/spf13/cobra"
)

func TestCLIUpdateFloorWarnsBlocksAndLeavesRecoveryAccessible(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "policy.json")
	deadline := time.Now().Add(time.Hour)
	policy := api.UpdatePolicy{Schema: "paperboat.client-update-policy/v1", Revision: 1, MinimumVersion: "2026.10.08.2", Reason: "Protocol maintenance", EnforceAt: &deadline}
	calls := 0
	fetch := func(context.Context, string, *http.Client) (api.UpdatePolicy, error) { calls++; return policy, nil }
	ctx := withUpdatePolicyDependencies(t.Context(), func(d *updatePolicyDependencies) {
		d.distribution = "official"
		d.cachePath = func(string, string) string { return cache }
		d.version = func() string { return "2026.10.08.1" }
		d.fetch = func(ctx context.Context, issuer string, client *http.Client) (api.UpdatePolicy, error) {
			return fetch(ctx, issuer, client)
		}
	})
	command := &cobra.Command{Use: "pb"}
	command.SetContext(ctx)
	command.Flags().String("config", filepath.Join(t.TempDir(), "config.json"), "")
	command.Flags().String("server", "https://example.invalid", "")
	command.Flags().Bool("json", false, "")
	var stderr bytes.Buffer
	command.SetErr(&stderr)
	if err := checkCLIUpdatePolicy(command); err != nil || !bytes.Contains(stderr.Bytes(), []byte("before")) {
		t.Fatalf("advance warning: %q %v", stderr.String(), err)
	}
	// F5-07: within cliUpdatePolicyFresh the cached policy answers; no request per command.
	before := calls
	if err := checkCLIUpdatePolicy(command); err != nil || calls != before {
		t.Fatalf("fresh cache refetched: calls %d -> %d, %v", before, calls, err)
	}
	stale := time.Now().Add(-cliUpdatePolicyFresh - time.Minute)
	if err := os.Chtimes(cache, stale, stale); err != nil {
		t.Fatal(err)
	}
	policy.EnforceAt = nil
	policy.Revision++
	stderr.Reset()
	err := checkCLIUpdatePolicy(command)
	var apiError *api.APIError
	if !errors.As(err, &apiError) || apiError.Code != "update_required" || !bytes.Contains(stderr.Bytes(), []byte("Update required")) {
		t.Fatalf("hard banner: %q %v", stderr.String(), err)
	}
	for _, name := range []string{"update", "login", "logout", "switch", "doctor", "uninstall", "close", "delete", "cancel"} {
		child := &cobra.Command{Use: name}
		command.AddCommand(child)
		before := calls
		if err := checkCLIUpdatePolicy(child); err != nil || calls != before {
			t.Fatalf("recovery %s gated: %v", name, err)
		}
	}
	fetch = func(context.Context, string, *http.Client) (api.UpdatePolicy, error) {
		return api.UpdatePolicy{}, errors.New("offline")
	}
	if err := os.Chtimes(cache, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := checkCLIUpdatePolicy(command); !errors.As(err, &apiError) {
		t.Fatal("known unsupported policy forgotten while offline")
	}
	command.Flags().Set("json", "true")
	stderr.Reset()
	if err := checkCLIUpdatePolicy(command); err == nil || stderr.Len() != 0 {
		t.Fatalf("JSON presentation contaminated: %q %v", stderr.String(), err)
	}
}

func TestCLIUpdateFloorExemptsCustomBuilds(t *testing.T) {
	for _, distribution := range []string{"custom", ""} {
		ctx := withUpdatePolicyDependencies(t.Context(), func(d *updatePolicyDependencies) {
			d.distribution = distribution
			d.fetch = func(context.Context, string, *http.Client) (api.UpdatePolicy, error) {
				t.Fatal("custom build fetched official minimum-version policy")
				return api.UpdatePolicy{}, nil
			}
		})
		command := &cobra.Command{Use: "pb"}
		command.SetContext(ctx)
		if err := checkCLIUpdatePolicy(command); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLIUpdateRequiredHasActionableFailure(t *testing.T) {
	err := &api.APIError{Status: http.StatusUpgradeRequired, Code: "update_required"}
	value := classifyCLIJSONError(err)
	if value.Code != "update_required" || value.Category != "conflict" || value.StateChanged != false || value.Retryable || value.Recovery == "" || !bytes.Contains([]byte(value.Message), []byte("pb update")) {
		t.Fatalf("update requirement lost in error presentation: %+v", value)
	}
	if message := commandFailureMessage(err, classifyCommandFailure(err)); !bytes.Contains([]byte(message), []byte("pb update")) {
		t.Fatal(message)
	}
}

func TestUpdateNoticeSkipsSSHConnections(t *testing.T) {
	root := &cobra.Command{Use: "pb"}
	ssh := &cobra.Command{Use: "ssh"}
	doctor := &cobra.Command{Use: "doctor"}
	root.AddCommand(ssh)
	ssh.AddCommand(doctor)
	if !sshConnectionCommand(ssh) || sshConnectionCommand(doctor) || sshConnectionCommand(root) {
		t.Fatal("only `pb ssh` itself skips the updater notice")
	}
}
