package main

import (
	"bytes"
	"context"
	"flag"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

func TestInstallBrowserAccessIsOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	var out bytes.Buffer
	_ = run(context.Background(), []string{"install", "--help"}, &out)
	if !regexp.MustCompile(`-browser-access[^\n]*\n[^-]*--browser-access=false[^\n]*\(default true\)`).MatchString(out.String()) {
		t.Fatalf("install help does not show browser access on by default with its off switch:\n%s", out.String())
	}
	for _, c := range []struct {
		args []string
		want bool
	}{{nil, true}, {[]string{"--browser-access=false"}, false}, {[]string{"--browser-access"}, true}} {
		fs := installTestFlags()
		if err := fs.Parse(append([]string{"--endpoint-host", "203.0.113.10"}, c.args...)); err != nil {
			t.Fatal(err)
		}
		cfg, err := freshInstallConfig(context.Background(), fs, "tunnel", "selfhost")
		if err != nil || cfg.BrowserAccess != c.want {
			t.Fatalf("args %v: browser access %v, err %v, want %v", c.args, cfg.BrowserAccess, err, c.want)
		}
	}
}

// An installation recorded before browser access defaulted on keeps it off on upgrade; only a reinstall changes it.
func TestInstallUpgradeKeepsRecordedBrowserAccessOverNewDefault(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "protected")
		cfg := selfhost.Config{Name: "n", EndpointHost: "203.0.113.10", Listen: "127.0.0.1:38443", BrowserAccess: recorded, Components: []selfhost.Component{
			{Capability: "tunnel", TCPPort: 31443, QUICPort: 31444, Region: "selfhost", FailureDomain: "d"},
		}}
		if err := selfhost.Initialize(dir, cfg); err != nil {
			t.Fatal(err)
		}
		fs := installTestFlags()
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		got, existing, err := existingInstallConfig(dir, fs)
		if err != nil || !existing || got.BrowserAccess != recorded {
			t.Fatalf("recorded %v: upgrade read browser access %v (existing %v, err %v)", recorded, got.BrowserAccess, existing, err)
		}
	}
}

func TestInstallNamesMissingEdgeDomainsForBrowserAccess(t *testing.T) {
	edge := []selfhost.Component{{Capability: "tunnel"}}
	for _, c := range []struct {
		cfg  selfhost.Config
		want []string
	}{
		{selfhost.Config{BrowserAccess: true, Components: edge}, []string{"--preview-domain or --tunnel-domain", "--browser-access=false"}},
		{selfhost.Config{BrowserAccess: true, PreviewDomain: "p.example.test", Components: edge}, []string{"no --tunnel-domain"}},
		{selfhost.Config{BrowserAccess: true, PreviewDomain: "p.example.test", TunnelDomain: "t.example.test", Components: edge}, nil},
		{selfhost.Config{BrowserAccess: false, Components: edge}, nil},
		{selfhost.Config{BrowserAccess: true, Components: []selfhost.Component{{Capability: "relay"}}}, nil},
	} {
		notice := browserAccessNotice(c.cfg)
		if (notice == "") != (c.want == nil) {
			t.Fatalf("%+v: notice %q", c.cfg, notice)
		}
		for _, w := range c.want {
			if !strings.Contains(notice, w) {
				t.Fatalf("%+v: notice %q lacks %q", c.cfg, notice, w)
			}
		}
	}
}

func installTestFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	for _, n := range []string{"endpoint-host", "name", "claim-listen", "preview-domain", "tunnel-domain", "runtime-domain", "failure-domain", "region"} {
		fs.String(n, "", "")
	}
	fs.Bool("browser-access", true, "")
	for _, n := range []string{"relay", "edge", "both"} {
		fs.Bool(n, false, "")
	}
	return fs
}
