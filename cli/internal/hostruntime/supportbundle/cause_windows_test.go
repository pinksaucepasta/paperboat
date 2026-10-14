//go:build windows

package supportbundle

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsBundleCleanupNativeCauseAndRecovery(t *testing.T) {
	dir := realTempDir(t)
	output := filepath.Join(dir, "bundle.json")
	b := mustBuilder(t)
	p, err := b.Preview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b.beforePublish = func(string) error { return windows.ERROR_ACCESS_DENIED }
	_, err = b.Write(t.Context(), p, output)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || errorCode(err) != ErrorWriteFailed {
		t.Fatal("native primary cause missing")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed publish created output")
	}
	assertNoTemporaryFiles(t, dir)
	nativeErr := markWindowsFileForDeletion(windows.InvalidHandle)
	combined := retainCleanup(&Error{Code: ErrorWriteFailed, Operation: "publish support bundle output", Cause: windows.ERROR_ACCESS_DENIED}, nativeErr)
	if !errors.Is(combined, windows.ERROR_ACCESS_DENIED) || !errors.Is(combined, windows.ERROR_INVALID_HANDLE) {
		t.Fatal("actual native cleanup cause missing")
	}
	_ = combined.Error()
	b.beforePublish = nil
	if _, err := b.Write(t.Context(), p, output); err != nil {
		t.Fatal(err)
	}
	assertNoTemporaryFiles(t, dir)
}
