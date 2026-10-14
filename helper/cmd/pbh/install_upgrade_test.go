package main

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat-helper/internal/containers"
	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

func TestInstallUpgradeRetainsProtectedSettingsWithoutRepeatingFlags(t *testing.T) {
	if err := containers.CheckHost(); err != nil {
		t.Skip("supported installer host required")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("installer Docker executable required")
	}
	dir := filepath.Join(t.TempDir(), "protected")
	cfg := selfhost.Config{Name: "original-node", EndpointHost: "203.0.113.10", Listen: "127.0.0.1:38443", PreviewDomain: "preview.example.test", TunnelDomain: "tunnel.example.test", RuntimeDomain: "runtime.example.test", Components: []selfhost.Component{
		{Capability: "relay", TCPPort: 31445, QUICPort: 31446, Region: "original-region", FailureDomain: "original-domain", CapacityLimit: 128},
		{Capability: "tunnel", TCPPort: 31443, QUICPort: 31444, Region: "original-region", FailureDomain: "original-domain", CapacityLimit: 128},
	}}
	if err := selfhost.Initialize(dir, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"install", "--no-service", "--state-dir", dir, "--relay-image", "example.test/relay:next", "--tunnel-image", "example.test/tunnel:next"}
	if err := run(context.Background(), args, io.Discard); err != nil {
		t.Fatalf("upgrade rejected existing installation: %v", err)
	}
	saved, err := selfhost.ReadConfig(dir)
	if err != nil || !reflect.DeepEqual(saved, cfg) {
		t.Fatal("upgrade changed retained installation settings")
	}
	after, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("upgrade replaced protected TLS identity")
	}
	images, err := containers.LoadImages(dir)
	if err != nil || images.Relay != "example.test/relay:next" || images.Tunnel != "example.test/tunnel:next" {
		t.Fatal("upgrade did not select new configured images")
	}
	args = append(args, "--name", "replacement-name")
	if err := run(context.Background(), args, io.Discard); err == nil {
		t.Fatal("image upgrade accepted a conflicting installation identity")
	}
	saved, err = selfhost.ReadConfig(dir)
	if err != nil || !reflect.DeepEqual(saved, cfg) {
		t.Fatal("rejected configuration change modified retained settings")
	}
}
