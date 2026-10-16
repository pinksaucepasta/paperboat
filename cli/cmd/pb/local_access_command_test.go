package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestLocalAccessCommandsApplyAndPersistDomainAndAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldApply := applyLocalAccessSettings
	var applied []config.LocalAccessConfig
	applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig) error {
		applied = append(applied, cloneLocalAccess(next))
		cfg.LocalAccess = cloneLocalAccess(next)
		return cfg.Save()
	}
	t.Cleanup(func() { applyLocalAccessSettings = oldApply })

	for _, args := range [][]string{
		{"config", "local-access", "domain", "set", "DEV.Example.COM"},
		{"config", "local-access", "alias", "set", "HP", "Jellyfin", "8989"},
		{"config", "local-access", "alias", "set", "hp", "jellyfin", "8096"},
	} {
		var stdout, stderr bytes.Buffer
		root := newRootCommand()
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(append([]string{"--config", path}, args...))
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("execute %v: %v; stderr=%q", args, err, stderr.String())
		}
		if strings.TrimSpace(stdout.String()) == "" {
			t.Fatalf("execute %v reported success without output", args)
		}
	}
	if len(applied) != 3 {
		t.Fatalf("apply calls = %d, want 3", len(applied))
	}
	if got := applied[0].Domain; got != "dev.example.com" {
		t.Fatalf("applied domain = %q", got)
	}
	if got := applied[2].ServiceAliases; len(got) != 1 || got[0] != (config.LocalServiceAlias{MachineAlias: "hp", Name: "jellyfin", Port: 8096}) {
		t.Fatalf("replacement alias = %+v", got)
	}

	var stdout bytes.Buffer
	root := newRootCommand()
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", path, "config", "local-access", "alias", "unset", "hp", "jellyfin"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalAccess.Domain != "dev.example.com" || len(cfg.LocalAccess.ServiceAliases) != 0 {
		t.Fatalf("persisted local access = %+v", cfg.LocalAccess)
	}
	if len(applied) != 4 || len(applied[3].ServiceAliases) != 0 {
		t.Fatalf("unset was not applied: %+v", applied)
	}
}

func TestLocalAccessApplyFailureDoesNotClaimSuccessOrChangeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAccess.Domain = "dev.example.com"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	oldApply := applyLocalAccessSettings
	applyErr := errors.New("PRIVATE_TRUST_PROVISIONING_FAILURE")
	applyLocalAccessSettings = func(context.Context, *config.Config, config.LocalAccessConfig) error { return applyErr }
	t.Cleanup(func() { applyLocalAccessSettings = oldApply })

	var stdout, stderr bytes.Buffer
	root := newRootCommand()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config", path, "config", "local-access", "domain", "set", "other.example.com"})
	err = root.ExecuteContext(context.Background())
	if !errors.Is(err, applyErr) {
		t.Fatalf("apply error = %v", err)
	}
	if err.Error() != "local browser settings could not be applied. Inspect them with `pb config local-access show` before retrying." || strings.Contains(err.Error(), "PRIVATE_TRUST_PROVISIONING_FAILURE") {
		t.Fatalf("apply error was not presented safely: %q", err.Error())
	}
	if strings.Contains(stdout.String(), "set to") || strings.Contains(stderr.String(), "set to") {
		t.Fatalf("failed apply claimed success: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	unchanged, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.LocalAccess.Domain != "dev.example.com" {
		t.Fatalf("failed apply persisted domain %q", unchanged.LocalAccess.Domain)
	}
}

func TestLocalAccessReportedFailureRetainsMixedCausesWithoutPrivateText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAccess.Domain = "dev.example.com"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	secretPath := filepath.Join(t.TempDir(), "PRIVATE_LOCAL_ACCESS_PATH")
	primary := errors.New("PRIVATE_LOCAL_ACCESS_PAYLOAD")
	causes := errors.Join(primary, &os.PathError{Op: "open", Path: secretPath, Err: syscall.EIO})
	priorApply := applyLocalAccessSettings
	applyLocalAccessSettings = func(context.Context, *config.Config, config.LocalAccessConfig) error { return causes }
	t.Cleanup(func() { applyLocalAccessSettings = priorApply })

	reference := "support_01234567-89ab-4def-8123-456789abcdef"
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restoreObserver()
	var stdout, stderr bytes.Buffer
	code := runWithReporter(supportref.WithContext(t.Context(), reference), []string{"--no-customization", "--config", path, "config", "local-access", "domain", "set", "other.example.com"}, &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("command exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "PRIVATE_LOCAL_ACCESS") {
		t.Fatalf("private cause reached command output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if len(faults) != 1 {
		t.Fatalf("final command fault count = %d, want one", len(faults))
	}
	fault := faults[0]
	if fault.Errno != int(syscall.EIO) || fault.SupportReference != reference {
		t.Fatalf("fault lost cause/reference: %+v", fault)
	}
	faultText := fault.Cause + fault.ErrorType + strings.Join(fault.ErrorChain, ",")
	if strings.Contains(faultText, "PRIVATE_LOCAL_ACCESS") {
		t.Fatalf("private cause reached fault projection: %+v", fault)
	}
	unchanged, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.LocalAccess.Domain != "dev.example.com" {
		t.Fatalf("failed apply persisted domain %q", unchanged.LocalAccess.Domain)
	}
}

type localAccessFailingWriter struct {
	err      error
	attempts bytes.Buffer
}

func (w *localAccessFailingWriter) Write(value []byte) (int, error) {
	_, _ = w.attempts.Write(value)
	return 0, w.err
}

func TestLocalAccessConfirmationWriteFailureKeepsAppliedStateAndCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	priorApply := applyLocalAccessSettings
	applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig) error {
		cfg.LocalAccess = cloneLocalAccess(next)
		return cfg.Save()
	}
	t.Cleanup(func() { applyLocalAccessSettings = priorApply })

	secretPath := filepath.Join(t.TempDir(), "PRIVATE_LOCAL_ACCESS_OUTPUT_PATH")
	writeErr := &os.PathError{Op: "write", Path: secretPath, Err: syscall.EIO}
	stdout := &localAccessFailingWriter{err: writeErr}
	var stderr bytes.Buffer
	reference := "support_01234567-89ab-4def-8123-456789abcdef"
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restoreObserver()
	code := runWithReporter(supportref.WithContext(t.Context(), reference), []string{"--no-customization", "--config", path, "config", "local-access", "domain", "set", "dev.example.com"}, stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("command exit code = %d, want 1", code)
	}
	if strings.Contains(stdout.attempts.String(), "PRIVATE_LOCAL_ACCESS_OUTPUT_PATH") || strings.Contains(stderr.String(), "PRIVATE_LOCAL_ACCESS_OUTPUT_PATH") {
		t.Fatalf("writer cause reached command output: attempted=%q stderr=%q", stdout.attempts.String(), stderr.String())
	}
	if !strings.Contains(stdout.attempts.String(), "Local browser domain set to dev.example.com.") {
		t.Fatalf("expected confirmation write was not attempted: %q", stdout.attempts.String())
	}
	updated, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LocalAccess.Domain != "dev.example.com" {
		t.Fatalf("successful apply was not persisted after confirmation write failure: %q", updated.LocalAccess.Domain)
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatalf("final output fault lost the write cause/reference or was captured more than once: %+v", faults)
	}
	faultText := faults[0].Cause + faults[0].ErrorType + strings.Join(faults[0].ErrorChain, ",")
	if strings.Contains(faultText, "PRIVATE_LOCAL_ACCESS_OUTPUT_PATH") {
		t.Fatalf("writer path reached fault projection: %+v", faults[0])
	}
}

func TestLocalAccessResetAndApplyUseEffectiveConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/client-configuration" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": "1", "local_access_base_domain": "reset.example.test", "machines_url": "https://console.example.test/machines"}})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = server.URL
	cfg.LocalAccess = config.LocalAccessConfig{
		Domain:         "dev.example.com",
		ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "hp", Name: "jellyfin", Port: 8989}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	oldApply := applyLocalAccessSettings
	var applied []config.LocalAccessConfig
	applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig) error {
		applied = append(applied, cloneLocalAccess(next))
		cfg.LocalAccess = cloneLocalAccess(next)
		return cfg.Save()
	}
	t.Cleanup(func() { applyLocalAccessSettings = oldApply })

	for _, args := range [][]string{{"config", "local-access", "domain", "reset"}} {
		root := newRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"--config", path}, args...))
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"local_access":{"domain":"other.example.com","service_aliases":[{"machine_alias":"hp","name":"jellyfin","port":8989}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", path, "config", "local-access", "apply"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Fatalf("apply calls = %d, want 2", len(applied))
	}
	if got := applied[0].Domain; got != "reset.example.test" {
		t.Fatalf("reset domain = %q", got)
	}
	if got := applied[0].ServiceAliases; len(got) != 1 || got[0].Name != "jellyfin" {
		t.Fatalf("reset unexpectedly changed aliases: %+v", got)
	}
	if applied[1].Domain != "other.example.com" || len(applied[1].ServiceAliases) != 1 {
		t.Fatalf("apply did not use the effective file configuration: %+v", applied[1])
	}
}

func TestLocalAccessInvalidValuesDoNotReachApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldApply := applyLocalAccessSettings
	calls := 0
	applyLocalAccessSettings = func(context.Context, *config.Config, config.LocalAccessConfig) error {
		calls++
		return nil
	}
	t.Cleanup(func() { applyLocalAccessSettings = oldApply })

	for _, args := range [][]string{
		{"config", "local-access", "domain", "set", "com"},
		{"config", "local-access", "alias", "set", "hp", "8989", "8989"},
		{"config", "local-access", "alias", "set", "hp", "jellyfin", "0"},
		{"config", "local-access", "alias", "set", "hp", "jellyfin", "65536"},
	} {
		root := newRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"--config", path}, args...))
		if err := root.ExecuteContext(context.Background()); err == nil {
			t.Fatalf("invalid arguments %v were accepted", args)
		}
	}
	if calls != 0 {
		t.Fatalf("apply called %d times for invalid values", calls)
	}
}

func TestLocalAccessConfigurationViewsExposeDomainAndAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAccess = config.LocalAccessConfig{
		Domain:         "dev.example.com",
		ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "studio", Name: "jellyfin", Port: 8989}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var jsonOutput bytes.Buffer
	root := newRootCommand()
	root.SetOut(&jsonOutput)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", path, "config", "show", "--json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var result struct {
		LocalAccess config.LocalAccessConfig `json:"local_access"`
	}
	if err := json.Unmarshal(jsonOutput.Bytes(), &result); err != nil {
		t.Fatalf("decode config show JSON: %v; output=%q", err, jsonOutput.String())
	}
	if result.LocalAccess.Domain != "dev.example.com" || len(result.LocalAccess.ServiceAliases) != 1 || result.LocalAccess.ServiceAliases[0].Name != "jellyfin" {
		t.Fatalf("config show local access = %+v", result.LocalAccess)
	}

	var humanOutput bytes.Buffer
	root = newRootCommand()
	root.SetOut(&humanOutput)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", path, "config", "local-access", "show"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(humanOutput.String(), "domain: dev.example.com") || !strings.Contains(humanOutput.String(), "jellyfin.studio -> configured port 8989") {
		t.Fatalf("human local-access view = %q", humanOutput.String())
	}
}

func TestLocalAccessCLICommandsAreDiscoverable(t *testing.T) {
	root := newRootCommand()
	for _, path := range []string{
		"config local-access domain set",
		"config local-access domain reset",
		"config local-access alias set",
		"config local-access alias unset",
		"config local-access show",
		"config local-access apply",
	} {
		command, _, err := root.Find(strings.Fields(path))
		if err != nil || command.CommandPath() != "pb "+path {
			t.Fatalf("find %q command=%v err=%v", path, command, err)
		}
	}
}

func TestLocalAccessDomainResetUsesServerDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		status       int
		wantError    bool
	}{
		{"server namespace", "PRIVATE.Example.TEST", http.StatusOK, false},
		{"missing namespace", "", http.StatusOK, true},
		{"invalid namespace", "bad/path", http.StatusOK, true},
		{"discovery unavailable", "private.example.test", http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/client-configuration" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected public discovery request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
					"version": "1", "local_access_base_domain": tc.domain,
					"machines_url": "https://console.example.test/machines",
				}})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "config.json")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ServerURL = server.URL
			cfg.LocalAccess.Domain = "custom.example.test"
			cfg.LocalAccess.ServiceAliases = []config.LocalServiceAlias{{MachineAlias: "target", Name: "kept", Port: 8080}}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			prior := applyLocalAccessSettings
			applies := 0
			applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig) error {
				applies++
				cfg.LocalAccess = cloneLocalAccess(next)
				return cfg.Save()
			}
			t.Cleanup(func() { applyLocalAccessSettings = prior })
			root := newRootCommand()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"--config", path, "config", "local-access", "domain", "reset", "--json"})
			err = root.ExecuteContext(t.Context())
			if (err != nil) != tc.wantError {
				t.Fatalf("reset error = %v", err)
			}
			if requests != 1 {
				t.Fatalf("discovery requests = %d", requests)
			}
			persisted, loadErr := config.Load(path)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wantDomain, wantApplies := "private.example.test", 1
			if tc.wantError {
				wantDomain, wantApplies = "custom.example.test", 0
			}
			if persisted.LocalAccess.Domain != wantDomain || applies != wantApplies {
				t.Fatalf("domain=%q applies=%d", persisted.LocalAccess.Domain, applies)
			}
			if len(persisted.LocalAccess.ServiceAliases) != 1 || persisted.LocalAccess.ServiceAliases[0].Name != "kept" {
				t.Fatal("reset changed unrelated service aliases")
			}
			if tc.wantError && output.Len() != 0 {
				t.Fatal("failed discovery claimed success")
			}
			if !tc.wantError {
				var receipt struct {
					OK   bool `json:"ok"`
					Data struct {
						Applied     bool                     `json:"applied"`
						LocalAccess config.LocalAccessConfig `json:"local_access"`
					} `json:"data"`
				}
				if err := json.Unmarshal(output.Bytes(), &receipt); err != nil {
					t.Fatal(err)
				}
				if !receipt.OK || !receipt.Data.Applied || receipt.Data.LocalAccess.Domain != wantDomain {
					t.Fatal("reset receipt lost the applied server namespace")
				}
			}
		})
	}
}
