package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

func sharedRingContext(t *testing.T) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	return withHostDependencies(t.Context(), func(d *hostDependencies) {
		d.daemonPaths = func() (localapi.Paths, error) { return localapi.Paths{StateRoot: root}, nil }
	}), filepath.Join(root, "diagnostics")
}

// Another pb process (a daemon on a slow disk, the SSH proxy exiting beside pb ssh) keeps writer.lock past the whole
// save time. The command succeeded, so the user hears nothing; the local failure file names the step.
func TestShutdownBehindAnotherWriterStaysQuietAndRecordsTheStep(t *testing.T) {
	ctx, directory := sharedRingContext(t)
	var stderr bytes.Buffer
	_, closeDiagnostics := openProcessDiagnostics(ctx, []string{"status"}, &errorreport.Reporter{}, &stderr)
	release := holdWriterLock(t, directory)
	defer release()
	closeDiagnostics()
	if stderr.Len() != 0 {
		t.Fatalf("waiting behind another writer was shown to the user: %s", stderr.String())
	}
	recorded, err := os.ReadFile(filepath.Join(directory, "last-diagnostics-failure.txt"))
	if err != nil || !strings.Contains(string(recorded), "storage") || !strings.Contains(string(recorded), "deadline") {
		t.Fatalf("failing step not recorded locally: %q %v", recorded, err)
	}
}

// The same lock held while a command starts must not cost it its disk diagnostics.
func TestStartBehindAnotherWriterKeepsDiskDiagnostics(t *testing.T) {
	ctx, directory := sharedRingContext(t)
	var stderr bytes.Buffer
	_, closeDiagnostics := openProcessDiagnostics(ctx, []string{"status"}, &errorreport.Reporter{}, &stderr)
	closeDiagnostics()
	release := holdWriterLock(t, directory)
	released := time.AfterFunc(2500*time.Millisecond, release)
	defer func() {
		if released.Stop() {
			release()
		}
	}()
	commandCtx, closeDiagnostics := openProcessDiagnostics(ctx, []string{"status"}, &errorreport.Reporter{}, &stderr)
	if err := diagnostics.FromContext(commandCtx).Record("test", "after_busy_start", "info", nil); err != nil {
		t.Fatal(err)
	}
	closeDiagnostics()
	if stderr.Len() != 0 {
		t.Fatalf("a busy ring at start was shown to the user: %s", stderr.String())
	}
	ring, err := diagnostics.NewDiskRing(diagnostics.DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	data, err := ring.ReadAll(t.Context(), diagnostics.DefaultMaximumBytes)
	if err != nil || !bytes.Contains(data, []byte("after_busy_start")) {
		t.Fatalf("record of a command started beside a busy writer was lost: %v", err)
	}
}

type stalledLogHandler struct {
	slog.Handler
	resume chan struct{}
}

func (h stalledLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h stalledLogHandler) Handle(context.Context, slog.Record) error {
	<-h.resume
	return nil
}

// The log and SDK flush has its own bound: when it uses all of it, saving still has time to wait out another writer.
func TestStalledExportDoesNotUseUpTheTimeToSave(t *testing.T) {
	ctx, directory := sharedRingContext(t)
	var stderr bytes.Buffer
	_, closeDiagnostics := openProcessDiagnostics(ctx, []string{"status"}, &errorreport.Reporter{}, &stderr)
	handler := stalledLogHandler{resume: make(chan struct{})}
	configured := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		close(handler.resume)
		drain, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = diagnosticlog.Flush(drain)
		slog.SetDefault(configured)
	})
	diagnosticlog.TryInfo("stalled")
	release := holdWriterLock(t, directory)
	var once sync.Once
	released := time.AfterFunc(2600*time.Millisecond, func() { once.Do(release) })
	defer func() {
		released.Stop()
		once.Do(release)
	}()
	closeDiagnostics()
	if stderr.Len() != 0 {
		t.Fatalf("stderr: %s", stderr.String())
	}
	ring, err := diagnostics.NewDiskRing(diagnostics.DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer ring.Close()
	data, err := ring.ReadAll(t.Context(), diagnostics.DefaultMaximumBytes)
	if err != nil || !bytes.Contains(data, []byte("telemetry_shutdown")) {
		t.Fatalf("shutdown record lost after a slow export: %q %v", data, err)
	}
	if recorded, err := os.ReadFile(filepath.Join(directory, "last-diagnostics-failure.txt")); err != nil || !strings.Contains(string(recorded), "queue") {
		t.Fatalf("stalled queue not recorded locally: %q %v", recorded, err)
	}
}

// A storage fault is still the user's to hear about.
func TestShutdownStorageFaultIsStillReported(t *testing.T) {
	ctx, directory := sharedRingContext(t)
	var stderr bytes.Buffer
	commandCtx, closeDiagnostics := openProcessDiagnostics(ctx, []string{"status"}, &errorreport.Reporter{}, &stderr)
	if err := os.Rename(directory, directory+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := diagnostics.FromContext(commandCtx).Record("test", "unsaved", "info", nil); err != nil {
		t.Fatal(err)
	}
	closeDiagnostics()
	if !strings.Contains(stderr.String(), "Some local diagnostics could not be saved before shutdown") {
		t.Fatalf("storage fault was hidden: %q", stderr.String())
	}
}
