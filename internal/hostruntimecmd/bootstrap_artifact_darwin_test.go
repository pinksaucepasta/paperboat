//go:build darwin

package hostruntimecmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeDarwinBootstrapArtifactDoesNotInstallPackage(t *testing.T) {
	root := t.TempDir()
	packagePath := filepath.Join(root, "paperboat.pkg")
	packageContents := []byte("signed package")
	if err := os.WriteFile(packagePath, packageContents, 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(root, "live-pb")
	if err := os.WriteFile(livePath, []byte("existing installation"), 0o700); err != nil {
		t.Fatal(err)
	}

	previousExtractor := extractDarwinBootstrapPackage
	previousPackageVerifier := verifyDarwinBootstrapPackage
	previousVerifier := verifyDarwinBootstrapExecutable
	extractions := 0
	extractDarwinBootstrapPackage = func(_ context.Context, _ string, extractionRoot string) (string, error) {
		extractions++
		payload := filepath.Join(extractionRoot, "expanded", "Payload", "pb")
		if err := os.MkdirAll(filepath.Dir(payload), 0o700); err != nil {
			return "", err
		}
		return payload, os.WriteFile(payload, []byte("new executable"), 0o700)
	}
	verifyDarwinBootstrapExecutable = func(_ context.Context, path string) error {
		_, err := os.Stat(path)
		return err
	}
	verifyDarwinBootstrapPackage = func(context.Context, string) error { return nil }
	t.Cleanup(func() {
		extractDarwinBootstrapPackage = previousExtractor
		verifyDarwinBootstrapPackage = previousPackageVerifier
		verifyDarwinBootstrapExecutable = previousVerifier
	})

	executable, err := materializeUnixBootstrapArtifact(context.Background(), packagePath)
	if err != nil {
		t.Fatal(err)
	}
	if executable != packagePath+".executable" {
		t.Fatalf("executable = %q", executable)
	}
	if body, err := os.ReadFile(executable); err != nil || string(body) != "new executable" {
		t.Fatalf("materialized executable=%q err=%v", body, err)
	}
	if _, err := materializeUnixBootstrapArtifact(context.Background(), packagePath); err != nil {
		t.Fatal(err)
	}
	if extractions != 2 {
		t.Fatalf("extractions = %d, want package validation on every retry", extractions)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".paperboat-bootstrap-") {
			t.Fatalf("temporary materialization path remains: %s", entry.Name())
		}
	}

	source, err := os.ReadFile("bootstrap_artifact_darwin.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "exec.Command") || strings.Contains(string(source), `"/usr/sbin/installer"`) {
		t.Fatal("bootstrap materialization invokes the global package installer")
	}
	gotPackage, err := os.ReadFile(packagePath)
	if err != nil || string(gotPackage) != string(packageContents) {
		t.Fatalf("package changed: contents=%q err=%v", gotPackage, err)
	}
	gotLive, err := os.ReadFile(livePath)
	if err != nil || string(gotLive) != "existing installation" {
		t.Fatalf("live installation changed: contents=%q err=%v", gotLive, err)
	}
}
