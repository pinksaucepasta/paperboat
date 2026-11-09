//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// A service that fails to start leaves the cause in its startup-error log, not only the error's type names.
func TestWindowsLaunchFailureLogNamesTheCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PaperboatUpdated-u1-startup-error.log")
	recordWindowsLaunchFailure(path, fmt.Errorf("validate updater installation binding: %w", errors.New("invalid Windows updater configuration")))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "\ncause: validate updater installation binding: invalid Windows updater configuration\n") {
		t.Fatalf("startup-error log lacks the cause: %s", body)
	}
}

// The updater's sweep loop ends as soon as no retired image is left (and removes what it finds on the way).
func TestSweepRetiredImagesLoopRemovesAndReturns(t *testing.T) {
	releases := filepath.Join(t.TempDir(), "releases")
	if err := os.MkdirAll(releases, 0o700); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(releases, "pb.rollback.exe.retired-a1")
	if err := os.WriteFile(retired, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		sweepRetiredImages(context.Background(), service.Layout{ReleasesRoot: releases})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep loop did not return although nothing is in use")
	}
	if _, err := os.Lstat(retired); !os.IsNotExist(err) {
		t.Fatalf("the retired image is still there: %v", err)
	}
}
