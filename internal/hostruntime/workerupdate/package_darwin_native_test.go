//go:build darwin

package workerupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesignature"
)

// Test the package inventory/extraction boundary with a real native fixture.
// Distribution signing is independently checked by the existing test below.
func TestDarwinManualPayloadExtraction(t *testing.T) {
	path := os.Getenv("PAPERBOAT_TEST_CANDIDATE_PKG")
	if path == "" {
		t.Skip("native package fixture not selected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	if _, err := ExtractDarwinPackage(ctx, path, root); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "expanded", "Payload", "usr", "local", "share", "man", "man1", "pb.1")
	body, err := os.ReadFile(page)
	if err != nil || !bytes.HasPrefix(body, []byte(".\\\" Paperboat managed manual v1\n")) {
		t.Fatalf("bundled manual missing: %v", err)
	}
}

// The fixture is a real PKG produced from the candidate pb executable. This
// exercises pkgutil without writing any live installation or service path.
func TestDarwinCandidatePackageExtraction(t *testing.T) {
	packagePath := os.Getenv("PAPERBOAT_TEST_CANDIDATE_PKG")
	if packagePath == "" {
		t.Skip("PAPERBOAT_TEST_CANDIDATE_PKG is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := nativesignature.New(nil).Verify(ctx, packagePath, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	executable, err := ExtractDarwinPackage(ctx, packagePath, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := binarytarget.Validate(executable, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	if err := nativesignature.New(nil).Verify(ctx, executable, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	cli, err := localReleaseIdentity(Release{}, executable)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "expanded", "Payload", "Library", "PrivilegedHelperTools", "Paperboat", "bin", "pb")
	if executable != helper {
		t.Fatalf("extracted executable = %q, want canonical helper %q", executable, helper)
	}
	cliLink := filepath.Join(root, "expanded", "Payload", "usr", "local", "bin", "pb")
	linkTarget, err := os.Readlink(cliLink)
	if err != nil || linkTarget != darwinPackageCLITarget {
		t.Fatalf("CLI link target = %q, err=%v", linkTarget, err)
	}
	contents, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	if hex.EncodeToString(digest[:]) != cli.SHA256 || int64(len(contents)) != cli.Length {
		t.Fatal("canonical executable digest or length changed during extraction")
	}
	t.Logf("verified extracted payload bytes=%d", cli.Length)
}
