//go:build darwin

package hostruntimecmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesignature"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

var errDarwinBootstrapPackage = errors.New("invalid macOS Paperboat package")

var extractDarwinBootstrapPackage = workerupdate.ExtractDarwinPackage
var verifyDarwinBootstrapPackage = func(ctx context.Context, path string) error {
	return nativesignature.New(nil).Verify(ctx, path, "darwin", "arm64")
}
var verifyDarwinBootstrapExecutable = validateDarwinBootstrapExecutable

// materializeUnixBootstrapArtifact verifies and extracts the canonical
// executable from a signed PKG. It never invokes installer(8) or writes an
// installation path; hostinstall owns the durable transaction and cutover.
func materializeUnixBootstrapArtifact(ctx context.Context, packagePath string) (string, error) {
	if packagePath == "" || !filepath.IsAbs(packagePath) || filepath.Clean(packagePath) != packagePath || !strings.EqualFold(filepath.Ext(packagePath), ".pkg") {
		return "", errDarwinBootstrapPackage
	}
	info, err := os.Lstat(packagePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errDarwinBootstrapPackage
	}
	if err := verifyDarwinBootstrapPackage(ctx, packagePath); err != nil {
		return "", err
	}
	executable := packagePath + ".executable"
	extractionRoot, err := os.MkdirTemp(filepath.Dir(packagePath), ".paperboat-bootstrap-package-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(extractionRoot)
	payload, err := extractDarwinBootstrapPackage(ctx, packagePath, extractionRoot)
	if err != nil {
		return "", err
	}
	if err := verifyDarwinBootstrapExecutable(ctx, payload); err != nil {
		return "", err
	}
	if err := verifyDarwinBootstrapExecutable(ctx, executable); err == nil {
		matches, compareErr := sameFileContents(payload, executable)
		if compareErr != nil {
			return "", compareErr
		}
		if matches {
			return executable, nil
		}
	}
	input, err := os.Open(payload)
	if err != nil {
		return "", err
	}
	defer input.Close()
	pending, err := os.CreateTemp(filepath.Dir(packagePath), ".paperboat-bootstrap-executable-")
	if err != nil {
		return "", err
	}
	pendingPath := pending.Name()
	defer os.Remove(pendingPath)
	if err := pending.Chmod(0o700); err != nil {
		pending.Close()
		return "", err
	}
	if _, err := io.Copy(pending, input); err != nil {
		pending.Close()
		return "", err
	}
	if err := pending.Sync(); err != nil {
		pending.Close()
		return "", err
	}
	if err := pending.Close(); err != nil {
		return "", err
	}
	if err := verifyDarwinBootstrapExecutable(ctx, pendingPath); err != nil {
		return "", err
	}
	if err := os.Rename(pendingPath, executable); err != nil {
		return "", err
	}
	return executable, nil
}

func sameFileContents(firstPath, secondPath string) (bool, error) {
	first, err := os.Open(firstPath)
	if err != nil {
		return false, err
	}
	defer first.Close()
	second, err := os.Open(secondPath)
	if err != nil {
		return false, err
	}
	defer second.Close()
	firstInfo, err := first.Stat()
	if err != nil {
		return false, err
	}
	secondInfo, err := second.Stat()
	if err != nil {
		return false, err
	}
	if firstInfo.Size() != secondInfo.Size() {
		return false, nil
	}
	firstHash, secondHash := sha256.New(), sha256.New()
	if _, err := io.Copy(firstHash, first); err != nil {
		return false, err
	}
	if _, err := io.Copy(secondHash, second); err != nil {
		return false, err
	}
	return string(firstHash.Sum(nil)) == string(secondHash.Sum(nil)), nil
}

func validateDarwinBootstrapExecutable(ctx context.Context, path string) error {
	if err := binarytarget.Validate(path, "darwin", "arm64"); err != nil {
		return err
	}
	return nativesignature.New(nil).Verify(ctx, path, "darwin", "arm64")
}
