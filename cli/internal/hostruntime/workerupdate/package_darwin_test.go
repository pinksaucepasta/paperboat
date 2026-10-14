//go:build darwin

package workerupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExtractDarwinPackageRealArchiveAndCancellation(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	helper := filepath.Join(payload, "Library/PrivilegedHelperTools/Paperboat/bin/pb")
	cli := filepath.Join(payload, "usr/local/bin/pb")
	for _, path := range []string{helper, cli} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(helper, []byte("bounded executable fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(darwinPackageCLITarget, cli); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "fixture.pkg")
	command := exec.Command("/usr/bin/pkgbuild", "--root", payload, "--identifier", darwinPackageIdentifier, "--version", "1", "--install-location", "/", pkg)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pkgbuild: %v: %.4096s", err, output)
	}
	for _, cancelled := range []bool{false, true} {
		extraction := filepath.Join(t.TempDir(), "extraction")
		if err := os.Mkdir(extraction, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		defer cancel()
		got, err := ExtractDarwinPackage(ctx, pkg, extraction)
		if cancelled {
			if err == nil {
				t.Fatal("cancelled extraction succeeded")
			}
			entries, _ := os.ReadDir(extraction)
			if len(entries) != 0 {
				t.Fatal("cancelled extraction retained files")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(got)
			if err != nil || string(data) != "bounded executable fixture" {
				t.Fatalf("payload=%q error=%v", data, err)
			}
		}
	}
}
