package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/theupdateframework/go-tuf/v2/metadata"
)

func publishTestRelease(t *testing.T, repository, version string) {
	t.Helper()
	artifacts := t.TempDir()
	for _, target := range supportedReleaseTargets() {
		name := releaseAssetName(target.platform, target.architecture)
		if err := os.WriteFile(filepath.Join(artifacts, name), []byte("asset "+version+" "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	evidence := map[string]string{}
	for _, architecture := range []string{"amd64", "arm64"} {
		body, _ := json.Marshal(windowsNativeQualification{Schema: windowsNativeQualificationSchema, ReleaseVersion: version, Platform: "windows", Architecture: architecture, Status: "passed", NativeTested: true, WindowsBuild: "26100", Runner: "windows-" + architecture + "-test"})
		path := filepath.Join(artifacts, windowsNativeQualificationTarget(architecture))
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		evidence[architecture] = path
	}
	if err := publish(repository, version, artifacts, evidence, 1, "routine", false); err != nil {
		t.Fatal(err)
	}
}

// The self-host bundle becomes two signed targets next to pb's: verify-published accepts them, a later pb release
// keeps them, and the next bundle replaces them.
func TestSelfhostBundleTargetsAreSignedAndKept(t *testing.T) {
	t.Setenv("PAPERBOAT_TUF_CI", "1")
	repository := filepath.Join(t.TempDir(), "repository")
	if err := initializeTestRepository(t, repository); err != nil {
		t.Fatal(err)
	}
	publishTestRelease(t, repository, "2026.11.19.1")
	bundles := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(bundles, "paperboat-selfhost-linux-"+arch+".tar.gz"), []byte("bundle "+arch), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := addSelfhost(repository, "2026.11.19.1", bundles); err != nil {
		t.Fatal(err)
	}
	if err := verifyPublished(repository); err != nil {
		t.Fatalf("published set with the self-host bundle: %v", err)
	}
	signed := func() map[string]*metadata.TargetFiles {
		targets, err := metadata.Targets().FromFile(filepath.Join(repository, "metadata", "targets.json"))
		if err != nil {
			t.Fatal(err)
		}
		return targets.Signed.Targets
	}
	target := signed()["selfhost/2026.11.19.1/paperboat-selfhost-linux-arm64.tar.gz"]
	if target == nil || target.Custom == nil {
		t.Fatal("self-host bundle target missing")
	}
	var custom selfhostTargetCustom
	if err := json.Unmarshal(*target.Custom, &custom); err != nil || custom.URL != "https://github.com/"+releaseRepository()+"/releases/download/selfhost-2026.11.19.1/paperboat-selfhost-linux-arm64.tar.gz" {
		t.Fatalf("self-host target download URL = %+v, %v", custom, err)
	}
	unsigned := *target
	unsigned.Custom = nil
	if validateSelfhostTarget("selfhost/2026.11.19.1/paperboat-selfhost-linux-arm64.tar.gz", &unsigned) == nil {
		t.Fatal("a self-host target without its download URL was accepted")
	}
	publishTestRelease(t, repository, "2026.11.19.2")
	if signed()["selfhost/2026.11.19.1/paperboat-selfhost-linux-amd64.tar.gz"] == nil {
		t.Fatal("a pb release dropped the self-host bundle target")
	}
	if err := addSelfhost(repository, "2026.11.19.2", bundles); err != nil {
		t.Fatal(err)
	}
	if targets := signed(); targets["selfhost/2026.11.19.1/paperboat-selfhost-linux-amd64.tar.gz"] != nil || targets["selfhost/2026.11.19.2/paperboat-selfhost-linux-amd64.tar.gz"] == nil {
		t.Fatal("the next bundle did not replace the previous one")
	}
}
