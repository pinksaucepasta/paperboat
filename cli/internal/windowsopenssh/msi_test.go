package windowsopenssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveMSI answers the download with body and approves exactly "approved installer".
func serveMSI(t *testing.T, body string, err error) string {
	t.Helper()
	previousOpen, previousMSI, previousCreate, previousDirectory := openMSI, compatibility.MSI, createProtectedDirectory, msiDirectory
	t.Cleanup(func() {
		openMSI, compatibility.MSI, createProtectedDirectory, msiDirectory = previousOpen, previousMSI, previousCreate, previousDirectory
	})
	sum := sha256.Sum256([]byte("approved installer"))
	compatibility.MSI = map[string]MSIPackage{"amd64": {URL: "https://example.test/openssh.msi", SHA256: hex.EncodeToString(sum[:])}}
	root := t.TempDir()
	msiDirectory = func() (string, error) { return filepath.Join(root, "openssh-installer"), nil }
	createProtectedDirectory = func(path string) error { return os.Mkdir(path, 0o700) } // the test user is not elevated
	openMSI = func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), err
	}
	return root
}
