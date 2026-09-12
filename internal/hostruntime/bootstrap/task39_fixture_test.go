package bootstrap

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
)

// This opt-in publisher uses the same signed metadata fixture as verifier
// tests. Its root is injected only into task-owned test builds via Go overlay.
func TestTask39PublishNativeFixture(t *testing.T) {
	root := os.Getenv("PAPERBOAT_TASK39_FIXTURE_ROOT")
	if root == "" {
		t.Skip("task-owned native fixture only")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("absolute fixture directory required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PrivateKey{}
	keyPath := filepath.Join(root, "signing-keys.json")
	if raw, err := os.ReadFile(keyPath); err == nil {
		if json.Unmarshal(raw, &keys) != nil {
			t.Fatal("invalid test signer")
		}
		clear(raw)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	body := []byte("root-initialization")
	version := os.Getenv("PAPERBOAT_TASK39_VERSION")
	if version == "" {
		version = "2026.09.08.390"
	}
	platform, architecture := os.Getenv("PAPERBOAT_TASK39_PLATFORM"), os.Getenv("PAPERBOAT_TASK39_ARCH")
	if platform == "" {
		platform = "linux"
	}
	if architecture == "" {
		architecture = "amd64"
	}
	if path := os.Getenv("PAPERBOAT_TASK39_ARTIFACT"); path != "" {
		var err error
		body, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	metadataVersion := int64(1)
	if raw := os.Getenv("PAPERBOAT_TASK39_METADATA_VERSION"); raw != "" {
		var err error
		metadataVersion, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || metadataVersion < 1 {
			t.Fatal("positive PAPERBOAT_TASK39_METADATA_VERSION required")
		}
	}
	repo := newTestTUFRepositoryForMetadataVersion(t, body, version, time.Now().UTC().Add(24*time.Hour), platform, architecture, keys, metadataVersion, func(plan *releasepolicy.Plan) {
		// The native fixture checks isolation through three local health samples.
		plan.Activation.StabilityWindowSeconds = 2
		plan.Activation.StabilityProbeIntervalSeconds = 1
	})
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	clear(encoded)
	if _, err = os.Stat(filepath.Join(root, "trusted-root.json")); os.IsNotExist(err) {
		if err = os.WriteFile(filepath.Join(root, "trusted-root.json"), repo.root, 0600); err != nil {
			t.Fatal(err)
		}
	}
	directory := filepath.Join(root, "repository", platform+"-"+architecture, version)
	for name, value := range repo.files {
		path := filepath.Join(directory, name)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
