package main

import (
	"bytes"
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

func regionFlags(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("region", defaultRegion, "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestInstallRegionPrompt(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		input    string
		terminal bool
		want     string
		prompts  int
		fails    bool
	}{
		{"empty answer takes the default", nil, "\n", true, "selfhost", 1, false},
		{"custom value", nil, "eu-west\n", true, "eu-west", 1, false},
		{"invalid then valid", nil, "EU West\nus_east\n", true, "us_east", 2, false},
		{"three invalid answers fail", nil, "A\n1x\nbad name\n", true, "", 3, true},
		{"closed input fails", nil, "", true, "", 1, true},
		{"no prompt without a terminal", nil, "eu-west\n", false, "selfhost", 0, false},
		{"flag wins on a terminal", []string{"--region", "ap-south"}, "eu-west\n", true, "ap-south", 0, false},
		{"invalid flag is refused", []string{"--region", "Bad"}, "", false, "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := installRegion(regionFlags(t, c.args...), strings.NewReader(c.input), &out, c.terminal)
			if (err != nil) != c.fails || got != c.want {
				t.Fatalf("region %q, err %v; want %q, fails %v", got, err, c.want, c.fails)
			}
			if n := strings.Count(out.String(), "Region name for this server"); n != c.prompts {
				t.Fatalf("prompted %d times, want %d: %q", n, c.prompts, out.String())
			}
		})
	}
}

// An upgrade keeps the stored region: the same --region passes, a different one is refused, none is fine.
func TestUpgradeKeepsStoredRegion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "protected")
	cfg := selfhost.Config{Name: "n", EndpointHost: "203.0.113.10", Listen: "127.0.0.1:38443", Components: []selfhost.Component{
		{Capability: "relay", TCPPort: 31445, QUICPort: 31446, Region: "eu-west", FailureDomain: "d", CapacityLimit: 128},
	}}
	if err := selfhost.Initialize(dir, cfg); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args  []string
		fails bool
	}{{nil, false}, {[]string{"--region", "eu-west"}, false}, {[]string{"--region", "us-east"}, true}} {
		fs := flag.NewFlagSet("install", flag.ContinueOnError)
		fs.String("region", defaultRegion, "")
		for _, n := range []string{"endpoint-host", "name", "claim-listen", "preview-domain", "tunnel-domain", "runtime-domain", "browser-access", "failure-domain"} {
			fs.String(n, "", "")
		}
		fs.Bool("relay", false, "")
		fs.Bool("edge", false, "")
		fs.Bool("both", false, "")
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if _, existing, err := existingInstallConfig(dir, fs); !existing || (err != nil) != c.fails {
			t.Fatalf("args %v: existing %v, err %v, want fails %v", c.args, existing, err, c.fails)
		}
	}
}
