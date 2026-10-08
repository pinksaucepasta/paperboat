package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/spf13/cobra"
)

func TestCLIUpdateFloorWarnsBlocksAndLeavesRecoveryAccessible(t *testing.T) {
	priorDistribution := buildinfo.Distribution
	buildinfo.Distribution = "official"
	t.Cleanup(func() { buildinfo.Distribution = priorDistribution })
	priorFetch, priorVersion, priorCache := fetchCLIUpdatePolicy, cliUpdateVersion, cliUpdatePolicyCachePath
	t.Cleanup(func() {
		fetchCLIUpdatePolicy, cliUpdateVersion, cliUpdatePolicyCachePath = priorFetch, priorVersion, priorCache
	})
	cache := filepath.Join(t.TempDir(), "policy.json")
	cliUpdatePolicyCachePath = func(string, string) string { return cache }
	cliUpdateVersion = func() string { return "2026.10.08.1" }
	deadline := time.Now().Add(time.Hour)
	policy := api.UpdatePolicy{Schema: "paperboat.client-update-policy/v1", Revision: 1, MinimumVersion: "2026.10.08.2", Reason: "Protocol maintenance", EnforceAt: &deadline}
	calls := 0
	fetchCLIUpdatePolicy = func(context.Context, string, *http.Client) (api.UpdatePolicy, error) { calls++; return policy, nil }
	command := &cobra.Command{Use: "pb"}
	command.SetContext(context.Background())
	command.Flags().String("config", filepath.Join(t.TempDir(), "config.json"), "")
	command.Flags().String("server", "https://example.invalid", "")
	command.Flags().Bool("json", false, "")
	var stderr bytes.Buffer
	command.SetErr(&stderr)
	if err := checkCLIUpdatePolicy(command); err != nil || !bytes.Contains(stderr.Bytes(), []byte("before")) {
		t.Fatalf("advance warning: %q %v", stderr.String(), err)
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
	fetchCLIUpdatePolicy = func(context.Context, string, *http.Client) (api.UpdatePolicy, error) {
		return api.UpdatePolicy{}, errors.New("offline")
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
	priorDistribution, priorFetch := buildinfo.Distribution, fetchCLIUpdatePolicy
	t.Cleanup(func() { buildinfo.Distribution, fetchCLIUpdatePolicy = priorDistribution, priorFetch })
	fetchCLIUpdatePolicy = func(context.Context, string, *http.Client) (api.UpdatePolicy, error) {
		t.Fatal("custom build fetched official minimum-version policy")
		return api.UpdatePolicy{}, nil
	}
	for _, distribution := range []string{"custom", ""} {
		buildinfo.Distribution = distribution
		if err := checkCLIUpdatePolicy(&cobra.Command{Use: "pb"}); err != nil {
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
