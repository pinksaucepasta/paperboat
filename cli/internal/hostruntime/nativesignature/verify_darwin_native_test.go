//go:build darwin

package nativesignature

import (
	"context"
	"debug/macho"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Exercise the actual codesign validator on a staged copy without touching the
// installed executable or changing Gatekeeper policy.
func TestNativeDarwinCodeSignatureSurvivesStagingCopyAndRejectsCorruption(t *testing.T) {
	source := os.Getenv("PAPERBOAT_NATIVE_TEST_ARTIFACT")
	if source == "" {
		t.Skip("PAPERBOAT_NATIVE_TEST_ARTIFACT is not set")
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 256<<20 {
		t.Fatalf("invalid native artifact: %v", err)
	}
	target := filepath.Join(t.TempDir(), "paperboat-runtime")
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, io.LimitReader(input, info.Size()+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("stage native artifact: %v", errors.Join(copyErr, closeErr))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := New(nil).Verify(ctx, target, "darwin", runtime.GOARCH); err != nil {
		t.Fatalf("intact staged code signature: %v", err)
	}

	image, err := macho.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	section := image.Section("__text")
	if section == nil || section.Size == 0 || int64(section.Offset) >= info.Size() {
		_ = image.Close()
		t.Fatal("native artifact has no signature-covered executable text")
	}
	offset := int64(section.Offset)
	if err := image.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var original [1]byte
	_, readErr := file.ReadAt(original[:], offset)
	original[0] ^= 1
	var writeErr error
	if readErr == nil {
		_, writeErr = file.WriteAt(original[:], offset)
	}
	closeErr = file.Close()
	if err := errors.Join(readErr, writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if err := New(nil).Verify(ctx, target, "darwin", runtime.GOARCH); !errors.Is(err, ErrInvalid) {
		t.Fatalf("modified signature-covered executable text accepted: %v", err)
	}
}
