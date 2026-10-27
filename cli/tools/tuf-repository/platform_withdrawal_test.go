package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

func TestPublicationReplacesWithdrawnProducts(t *testing.T) {
	for _, damage := range []string{"valid", "unsigned", "malformed"} {
		t.Run(damage, func(t *testing.T) {
			repo := threeProductRepository(t)
			_, prior, _, _, err := loadSet(repo)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "unsigned":
				prior.ClearSignatures()
			case "malformed":
				raw := json.RawMessage(`{"schema":"invalid"}`)
				prior.Signed.Targets["pb-darwin-arm64"].Custom = &raw
				prior.ClearSignatures()
				if err := sign(prior); err != nil {
					t.Fatal(err)
				}
			}
			writePriorTargets(t, repo, prior)
			path := filepath.Join(repo, "metadata", "targets.json")
			before := readTestMetadata(t, path)
			verifyErr := verifyPublished(repo)
			artifacts := t.TempDir()
			for _, name := range []string{"pb-linux-amd64", "pb-linux-arm64"} {
				if err := os.WriteFile(filepath.Join(artifacts, name), []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = publish(repo, "2026.08.22.16", artifacts, nil, 2, "routine", false)
			if damage != "valid" {
				if verifyErr == nil || err == nil {
					t.Fatalf("%s prior metadata accepted: verify=%v publish=%v", damage, verifyErr, err)
				}
				want := "verify existing targets"
				if damage == "malformed" {
					want = "invalid schema"
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s rejected for wrong reason: %v", damage, err)
				}
				if !bytes.Equal(before, readTestMetadata(t, path)) {
					t.Fatal("rejected publication changed prior metadata")
				}
				return
			}
			if verifyErr != nil || err != nil {
				t.Fatalf("three-product prior set to Linux publication: verify=%v publish=%v", verifyErr, err)
			}
			if err := verifyPublished(repo); err != nil {
				t.Fatal(err)
			}
			_, after, _, _, err := loadSet(repo)
			if err != nil || len(after.Signed.Targets) != 2 {
				t.Fatalf("new release must contain exactly two Linux products: %v", err)
			}
			for _, name := range []string{"pb-linux-amd64", "pb-linux-arm64"} {
				info := after.Signed.Targets[name]
				if info == nil || info.Custom == nil {
					t.Fatalf("missing new Linux target %s", name)
				}
				custom, err := decodeAndValidateAssetTargetCustom(*info.Custom, time.Now().UTC())
				if err != nil || custom.Version != "2026.08.22.16" {
					t.Fatalf("new target %s version=%s error=%v", name, custom.Version, err)
				}
			}
		})
	}
}

// Build prior signed metadata independently of the current publisher's product list.
func threeProductRepository(t *testing.T) string {
	t.Helper()
	t.Setenv("PAPERBOAT_TUF_CI", "1")
	repo := filepath.Join(t.TempDir(), "repository")
	if err := initializeTestRepository(t, repo); err != nil {
		t.Fatal(err)
	}
	_, targets, _, _, err := loadSet(repo)
	if err != nil {
		t.Fatal(err)
	}
	template := validAssetTargetCustomFixture(t, time.Now().UTC())
	platforms := []releasepolicy.PlatformTarget{
		{Platform: "darwin", Architecture: "arm64"},
		{Platform: "linux", Architecture: "amd64"},
		{Platform: "linux", Architecture: "arm64"},
	}
	plan, err := releasepolicy.Default(template.Version, template.ReleaseIndex.ManifestSHA256, 1, "routine", "prior-release", platforms)
	if err != nil {
		t.Fatal(err)
	}
	planDigest, err := plan.PlanSHA256()
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range platforms {
		custom := template
		custom.Platform, custom.Architecture = platform.Platform, platform.Architecture
		custom.Format = releaseAssetFormat(platform.Platform)
		custom.AssetName = releaseAssetName(platform.Platform, platform.Architecture)
		custom.URL = strings.TrimSuffix(template.URL, template.AssetName) + custom.AssetName
		index := &custom.ReleaseIndex
		index.Platform, index.Architecture, index.BinaryFormat = custom.Platform, custom.Architecture, custom.Format
		index.DeploymentPlan, index.DeploymentPlanSHA256 = &plan, planDigest
		component := template.ReleaseIndex.Targets[0]
		component.Platform, component.Architecture, component.BinaryFormat = custom.Platform, custom.Architecture, custom.Format
		component.TargetPath, component.AssetName, component.DownloadURL = custom.AssetName, custom.AssetName, custom.URL
		index.Targets = []componentTarget{component}
		body, err := json.Marshal(custom)
		if err != nil {
			t.Fatal(err)
		}
		raw := json.RawMessage(body)
		digest, err := hex.DecodeString(custom.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		targets.Signed.Targets[custom.AssetName] = &metadata.TargetFiles{Length: custom.Length, Hashes: metadata.Hashes{"sha256": digest}, Custom: &raw}
	}
	targets.ClearSignatures()
	if err := sign(targets); err != nil {
		t.Fatal(err)
	}
	writePriorTargets(t, repo, targets)
	return repo
}

func writePriorTargets(t *testing.T, repo string, targets *metadata.Metadata[metadata.TargetsType]) {
	t.Helper()
	for _, name := range []string{"targets.json", fmt.Sprintf("%d.targets.json", targets.Signed.Version)} {
		if err := targets.ToFile(filepath.Join(repo, "metadata", name), false); err != nil {
			t.Fatal(err)
		}
	}
}
