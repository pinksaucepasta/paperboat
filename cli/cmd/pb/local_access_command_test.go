package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
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
	applyErr := errors.New("trust provisioning failed")
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

func TestLocalAccessResetAndApplyUseEffectiveConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
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
	if got := applied[0].Domain; got != config.DefaultLocalAccessDomain {
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
