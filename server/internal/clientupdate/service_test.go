package clientupdate

import (
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/server/internal/releases"
	"testing"
	"time"
)

func TestSupportedVersion(t *testing.T) {
	for _, s := range []string{"2026.10.8.1", "2024.02.29.0", "2026.10.08.100"} {
		if _, ok := ParseVersion(s); !ok {
			t.Fatalf("valid %s", s)
		}
	}
	for _, s := range []string{"2026.2.29.1", "2026.13.1.1", "2026.10.8", "2026.10.8.-1", "2026.10.8.1-dev", "2026.10.8.4294967296", "0.1.1.1", "dev", ""} {
		if _, ok := ParseVersion(s); ok {
			t.Fatalf("invalid %s", s)
		}
	}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	p := Policy{MinimumVersion: "2026.10.8.10", EnforceAt: &at}
	if RequiresUpdate(p, "2026.10.8.9", at.Add(-time.Nanosecond)) {
		t.Fatal("enforced before date")
	}
	for _, s := range []string{"2026.10.8.9", "dev", ""} {
		if !RequiresUpdate(p, s, at) {
			t.Fatalf("below/malformed %s", s)
		}
	}
	for _, s := range []string{"2026.10.8.10", "2026.10.8.11", "2026.10.9.0"} {
		if RequiresUpdate(p, s, at) {
			t.Fatalf("compatible %s", s)
		}
	}
	if RequiresUpdate(Policy{}, "dev", at) {
		t.Fatal("disabled")
	}
}
func TestPolicyValidation(t *testing.T) {
	for _, in := range []Input{{ExpectedRevision: -1}, {MinimumVersion: "2026.10.8.1"}, {MinimumVersion: "2026.02.30.1", Reason: "x"}, {Reason: "disabled reason"}, {MinimumVersion: "2026.10.8.1", Reason: "x\n"}} {
		if validate(in) == nil {
			t.Fatalf("invalid %+v", in)
		}
	}
	if validate(Input{}) != nil || validate(Input{MinimumVersion: "2026.10.8.1", Reason: "Protocol support"}) != nil {
		t.Fatal("valid")
	}
}

func TestPublishedCeilingMixedPlatformVersions(t *testing.T) {
	current := releases.Current{Version: "2026.09.30.18", Assets: map[string]releases.Asset{
		"darwin-arm64":  {Version: "2026.09.30.18"},
		"linux-amd64":   {Version: "2026.09.05.0"},
		"linux-arm64":   {Version: "2026.09.30.18"},
		"windows-amd64": {Version: "2026.09.05.0"},
		"windows-arm64": {Version: "2026.09.05.0"},
	}}
	ceiling, err := publishedCeiling(current)
	if err != nil || ceiling != "2026.09.05.0" {
		t.Fatalf("ceiling=%s err=%v", ceiling, err)
	}
	value, _ := ParseVersion(ceiling)
	for _, floor := range []string{"2026.09.05.0", "2026.09.04.99"} {
		parsed, _ := ParseVersion(floor)
		if Compare(parsed, value) > 0 {
			t.Fatal("available floor rejected")
		}
	}
	for _, floor := range []string{"2026.09.05.1", "2026.09.30.18"} {
		parsed, _ := ParseVersion(floor)
		if Compare(parsed, value) <= 0 {
			t.Fatal("unavailable global floor accepted")
		}
	}
	current.Assets["linux-amd64"] = releases.Asset{Version: "malformed"}
	if _, err := publishedCeiling(current); err == nil {
		t.Fatal("malformed asset accepted")
	}
	if _, err := publishedCeiling(releases.Current{}); err == nil {
		t.Fatal("missing assets accepted")
	}
}

// The node floor's ceiling is the newest self-host version published for both Linux architectures.
func TestPublishedSelfhostCeilingNeedsEveryArchitecture(t *testing.T) {
	directory := t.TempDir()
	if _, err := publishedSelfhostCeiling(directory); err == nil {
		t.Fatal("ceiling without any publication")
	}
	publish := func(version string, arches ...string) {
		path := filepath.Join(directory, "selfhost", "versions", version)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, arch := range arches {
			if err := os.WriteFile(filepath.Join(path, "paperboat-selfhost-linux-"+arch+".tar.gz"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	publish("2026.11.16.2", "amd64", "arm64")
	publish("2026.11.18.1", "amd64")
	publish("not-a-version", "amd64", "arm64")
	if got, err := publishedSelfhostCeiling(directory); err != nil || got != "2026.11.16.2" {
		t.Fatalf("ceiling = %q, %v", got, err)
	}
}
