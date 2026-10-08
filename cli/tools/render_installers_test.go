package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRenderInstallersUsesEachVerifiedProductIdentity(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	for _, scenario := range []string{"retained-platforms", "wrong-url", "wrong-hash", "wrong-length", "extra-helper"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			targets := map[string]any{}
			for i, product := range []struct{ name, platform, arch, format string }{
				{"pb-linux-amd64", "linux", "amd64", "elf"},
				{"pb-linux-arm64", "linux", "arm64", "elf"},
				{"pb-darwin-arm64.pkg", "darwin", "arm64", "pkg"},
				{"pb-windows-amd64.exe", "windows", "amd64", "pe"},
				{"pb-windows-arm64.exe", "windows", "arm64", "pe"},
			} {
				version := "2026.10.08.21"
				if product.platform != "darwin" && product.arch == "arm64" {
					version = "2026.09.05.0"
				}
				digest := fmt.Sprintf("%064x", i+1)
				custom := map[string]any{"schema": "paperboat.tuf-asset/v1", "kind": "github-release-asset", "version": version, "platform": product.platform, "architecture": product.arch, "format": product.format, "asset_name": product.name, "repository": "pinksaucepasta/paperboat-cli", "url": "https://github.com/pinksaucepasta/paperboat-cli/releases/download/" + version + "/" + product.name, "sha256": digest, "length": 100 + i}
				targets[product.name] = map[string]any{"custom": custom, "hashes": map[string]any{"sha256": digest}, "length": 100 + i}
			}
			selected := targets["pb-linux-amd64"].(map[string]any)
			switch scenario {
			case "wrong-url":
				selected["custom"].(map[string]any)["url"] = "https://example.com/pb-linux-amd64"
			case "wrong-hash":
				selected["hashes"].(map[string]any)["sha256"] = strings.Repeat("f", 64)
			case "wrong-length":
				selected["length"] = 101
			case "extra-helper":
				targets["pb-bootstrap-linux-amd64"] = selected
			}
			body, err := json.Marshal(map[string]any{"signed": map[string]any{"targets": targets}})
			if err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(root, "targets.json")
			if err := os.WriteFile(metadata, body, 0600); err != nil {
				t.Fatal(err)
			}
			install, windows := filepath.Join(root, "install"), filepath.Join(root, "windows")
			output, err := exec.Command("python3", "render-installers.py", metadata, install, windows).CombinedOutput()
			if scenario != "retained-platforms" {
				if err == nil {
					t.Fatalf("invalid metadata was accepted: %s", output)
				}
				for _, path := range []string{install, windows} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("invalid metadata wrote %s", path)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("renderer: %v: %s", err, output)
			}
			for _, path := range []string{install, windows} {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if regexp.MustCompile(`@PAPERBOAT_[A-Z0-9_]+@`).Match(body) || strings.Contains(string(body), "pb-bootstrap") {
					t.Fatal("unrendered or helper installer")
				}
				for name, target := range targets {
					custom := target.(map[string]any)["custom"].(map[string]any)
					isWindows := custom["platform"] == "windows"
					if (path == windows) != isWindows {
						continue
					}
					for _, key := range []string{"version", "url", "sha256"} {
						if !strings.Contains(string(body), custom[key].(string)) {
							t.Fatalf("%s lacks %s %s", path, name, key)
						}
					}
				}
			}
		})
	}
}
