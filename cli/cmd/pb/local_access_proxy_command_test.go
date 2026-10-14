package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"path/filepath"
	"testing"
)

func TestLocalMachineProxyCommandsPreserveAliasesAndApplyBeforeSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalAccess.ServiceAliases = []config.LocalServiceAlias{{MachineAlias: "homelab", Name: "bob", Port: 8080}}
	if err = cfg.Save(); err != nil {
		t.Fatal(err)
	}
	prior := applyLocalAccessSettings
	fail := false
	calls := 0
	applyLocalAccessSettings = func(_ context.Context, c *config.Config, next config.LocalAccessConfig) error {
		calls++
		if fail {
			return errors.New("protected gateway unavailable")
		}
		c.LocalAccess = cloneLocalAccess(next)
		return c.Save()
	}
	t.Cleanup(func() { applyLocalAccessSettings = prior })
	run := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		r := newRootCommand()
		r.SetOut(&out)
		r.SetErr(&bytes.Buffer{})
		r.SetArgs(append([]string{"--config", path, "config", "local-access", "proxy"}, args...))
		e := r.ExecuteContext(t.Context())
		return out.String(), e
	}
	out, err := run("set", "HOMELAB", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Applied     bool                     `json:"applied"`
			LocalAccess config.LocalAccessConfig `json:"local_access"`
		}
	}
	if err = json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Data.Applied || len(envelope.Data.LocalAccess.MachineProxies) != 1 || envelope.Data.LocalAccess.MachineProxies[0] != (config.LocalMachineProxy{MachineAlias: "homelab", Port: 80}) {
		t.Fatalf("unexpected applied settings: %+v", envelope)
	}
	if _, err = run("set", "homelab", "8081"); err != nil {
		t.Fatal(err)
	}
	fail = true
	if out, err = run("set", "homelab", "8090"); err == nil || out != "" {
		t.Fatalf("failed apply reported success: %q %v", out, err)
	}
	fail = false
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalAccess.MachineProxies) != 1 || cfg.LocalAccess.MachineProxies[0].Port != 8081 {
		t.Fatal("failed apply changed settings")
	}
	before := calls
	for _, args := range [][]string{{"set", "homelab", "0"}, {"set", "homelab", "65536"}, {"set", "bad.machine", "80"}} {
		if _, err = run(args...); err == nil {
			t.Fatalf("accepted invalid setting %v", args)
		}
	}
	if calls != before {
		t.Fatal("invalid settings reached apply")
	}
	if _, err = run("unset", "homelab"); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalAccess.MachineProxies) != 0 || len(cfg.LocalAccess.ServiceAliases) != 1 || cfg.LocalAccess.ServiceAliases[0].Port != 8080 {
		t.Fatal("unset changed explicit alias")
	}
}
