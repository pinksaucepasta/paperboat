package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func TestLocalAccessCommandsApplyAndPersistDomainAndAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldApply := applyLocalAccessSettings
	var applied []config.LocalAccessConfig
	var appliedDomains []string
	applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig, domain string) error {
		applied = append(applied, cloneLocalAccess(next))
		appliedDomains = append(appliedDomains, domain)
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
	if got := appliedDomains[0]; got != "dev.example.com" {
		t.Fatalf("applied domain = %q", got)
	}
	if appliedDomains[1] != "" || appliedDomains[2] != "" {
		t.Fatalf("an alias change tried to change the domain: %q", appliedDomains)
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
	if len(cfg.LocalAccess.ServiceAliases) != 0 {
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
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	oldApply := applyLocalAccessSettings
	applyErr := errors.New("PRIVATE_TRUST_PROVISIONING_FAILURE")
	applyLocalAccessSettings = func(context.Context, *config.Config, config.LocalAccessConfig, string) error { return applyErr }
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
}

func TestLocalAccessReportedFailureRetainsMixedCausesWithoutPrivateText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	secretPath := filepath.Join(t.TempDir(), "PRIVATE_LOCAL_ACCESS_PATH")
	primary := errors.New("PRIVATE_LOCAL_ACCESS_PAYLOAD")
	causes := errors.Join(primary, &os.PathError{Op: "open", Path: secretPath, Err: syscall.EIO})
	priorApply := applyLocalAccessSettings
	applyLocalAccessSettings = func(context.Context, *config.Config, config.LocalAccessConfig, string) error { return causes }
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
	applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig, _ string) error {
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
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatalf("final output fault lost the write cause/reference or was captured more than once: %+v", faults)
	}
	faultText := faults[0].Cause + faults[0].ErrorType + strings.Join(faults[0].ErrorChain, ",")
	if strings.Contains(faultText, "PRIVATE_LOCAL_ACCESS_OUTPUT_PATH") {
		t.Fatalf("writer path reached fault projection: %+v", faults[0])
	}
}

// useGuardStatus makes the machine guard report status for the local access commands.
func useGuardStatus(t *testing.T, status machineguard.RuntimeStatus, err error) {
	t.Helper()
	prior := localAccessGuardStatus
	localAccessGuardStatus = func(context.Context) (machineguard.RuntimeStatus, error) { return status, err }
	t.Cleanup(func() { localAccessGuardStatus = prior })
}

func TestLocalAccessResetAndApplyUseTheGuardsDomain(t *testing.T) {
	useGuardStatus(t, machineguard.RuntimeStatus{BrowserDomain: "custom.example.test", DefaultBrowserDomain: "reset.example.test"}, nil)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAccess = config.LocalAccessConfig{ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "hp", Name: "jellyfin", Port: 8989}}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	oldApply := applyLocalAccessSettings
	var applied []config.LocalAccessConfig
	var appliedDomains []string
	applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig, domain string) error {
		applied = append(applied, cloneLocalAccess(next))
		appliedDomains = append(appliedDomains, domain)
		cfg.LocalAccess = cloneLocalAccess(next)
		return cfg.Save()
	}
	t.Cleanup(func() { applyLocalAccessSettings = oldApply })

	for _, args := range [][]string{{"config", "local-access", "domain", "reset"}, {"config", "local-access", "apply"}} {
		root := newRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"--config", path}, args...))
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
	}
	if len(applied) != 2 || appliedDomains[0] != "reset.example.test" || appliedDomains[1] != "" {
		t.Fatalf("applied domains = %q (reset must restore the guard's recorded server domain; apply must not change it)", appliedDomains)
	}
	if got := applied[0].ServiceAliases; len(got) != 1 || got[0].Name != "jellyfin" {
		t.Fatalf("reset unexpectedly changed aliases: %+v", got)
	}
}

// A config file written before the guard owned the domain still carries one; it is ignored.
func TestLocalAccessIgnoresAStaleDomainInTheConfigFile(t *testing.T) {
	useGuardStatus(t, machineguard.RuntimeStatus{BrowserDomain: "ppr.example.test", DefaultBrowserDomain: "ppr.example.test"}, nil)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"local_access":{"domain":"local.pprbt.example.test","service_aliases":[{"machine_alias":"hp","name":"jellyfin","port":8989}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	root := newRootCommand()
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", path, "config", "local-access", "show"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "domain: ppr.example.test") || strings.Contains(output.String(), "local.pprbt.example.test") {
		t.Fatalf("show reports the stale file value instead of the guard's domain: %q", output.String())
	}
}

func TestLocalAccessInvalidValuesDoNotReachApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldApply := applyLocalAccessSettings
	calls := 0
	applyLocalAccessSettings = func(context.Context, *config.Config, config.LocalAccessConfig, string) error {
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
	useGuardStatus(t, machineguard.RuntimeStatus{BrowserDomain: "dev.example.com"}, nil)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAccess = config.LocalAccessConfig{
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
	if len(result.LocalAccess.ServiceAliases) != 1 || result.LocalAccess.ServiceAliases[0].Name != "jellyfin" {
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

func TestLocalAccessDomainResetNeedsTheGuardsRecordedDomain(t *testing.T) {
	for name, tc := range map[string]struct {
		status    machineguard.RuntimeStatus
		err       error
		wantApply bool
	}{
		"guard records the server domain": {status: machineguard.RuntimeStatus{DefaultBrowserDomain: "private.example.test"}, wantApply: true},
		"guard records none":              {status: machineguard.RuntimeStatus{}},
		"guard unreachable":               {err: errors.New("guard unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			useGuardStatus(t, tc.status, tc.err)
			path := filepath.Join(t.TempDir(), "config.json")
			prior := applyLocalAccessSettings
			var appliedDomain string
			applies := 0
			applyLocalAccessSettings = func(_ context.Context, cfg *config.Config, next config.LocalAccessConfig, domain string) error {
				applies++
				appliedDomain = domain
				return nil
			}
			t.Cleanup(func() { applyLocalAccessSettings = prior })
			root := newRootCommand()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"--config", path, "config", "local-access", "domain", "reset", "--json"})
			err := root.ExecuteContext(t.Context())
			if (err != nil) == tc.wantApply {
				t.Fatalf("reset error = %v, want error=%v", err, !tc.wantApply)
			}
			if tc.wantApply && (applies != 1 || appliedDomain != "private.example.test") {
				t.Fatalf("applies=%d domain=%q", applies, appliedDomain)
			}
			if !tc.wantApply && (applies != 0 || output.Len() != 0) {
				t.Fatalf("failed reset applied=%d or claimed success: %q", applies, output.String())
			}
		})
	}
}
