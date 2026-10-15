package releases

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func qualifiedSubsetBundle(t *testing.T) (string, map[string]any) {
	t.Helper()
	directory := t.TempDir()
	writeReadyBundle(t, directory, false)
	body, err := os.ReadFile(filepath.Join(directory, "tuf/metadata/targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	targets := document["signed"].(map[string]any)["targets"].(map[string]any)
	target := targets["pb-linux-arm64"].(map[string]any)
	customBody, _ := json.Marshal(target["custom"])
	customBody = []byte(strings.ReplaceAll(string(customBody), testVersion, "2026.08.24.0"))
	var custom map[string]any
	if err := json.Unmarshal(customBody, &custom); err != nil {
		t.Fatal(err)
	}
	index := custom["release_index"].(map[string]any)
	plan, _ := json.Marshal(index["deployment_plan"])
	digest := sha256.Sum256(append(plan, '\n'))
	index["deployment_plan_sha256"] = hex.EncodeToString(digest[:])
	target["custom"] = custom
	return directory, document
}

func writeSubsetMetadata(t *testing.T, directory string, document map[string]any) {
	t.Helper()
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "tuf/metadata/targets.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadCurrentQualifiedSubset(t *testing.T) {
	directory, document := qualifiedSubsetBundle(t)
	writeSubsetMetadata(t, directory, document)
	current, err := ReadCurrent(directory)
	if err != nil {
		t.Fatalf("legitimate omitted platform version rejected: %v", err)
	}
	if current.Version != testVersion || current.Assets["pb-linux-arm64"].Version != "2026.08.24.0" || current.Assets["pb-linux-amd64"].Version != testVersion {
		t.Fatal("per-platform publication versions were not preserved")
	}
	if err := Ready(directory); err != nil {
		t.Fatalf("qualified subset not ready: %v", err)
	}
}

func TestReadCurrentQualifiedSubsetRejectsTampering(t *testing.T) {
	for _, kind := range []string{"hash", "repository", "version", "index", "missing_platform"} {
		t.Run(kind, func(t *testing.T) {
			directory, document := qualifiedSubsetBundle(t)
			targets := document["signed"].(map[string]any)["targets"].(map[string]any)
			target := targets["pb-linux-arm64"].(map[string]any)
			custom := target["custom"].(map[string]any)
			switch kind {
			case "hash":
				custom["sha256"] = strings.Repeat("b", 64)
			case "repository":
				custom["repository"] = "other/paperboat-cli"
			case "version":
				custom["version"] = testVersion
			case "index":
				custom["release_index"].(map[string]any)["version"] = testVersion
			case "missing_platform":
				delete(targets, "pb-linux-arm64")
			}
			writeSubsetMetadata(t, directory, document)
			if _, err := ReadCurrent(directory); err == nil {
				t.Fatalf("%s mismatch accepted", kind)
			}
		})
	}
}
