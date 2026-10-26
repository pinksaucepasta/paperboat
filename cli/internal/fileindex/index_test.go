package fileindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

const fileIndexSupportReference = "support_00000000-0000-4000-8000-000000000002"

func TestRefreshCachesAndDetectsDirectoryChanges(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "index.json")
	first := filepath.Join(root, "workspace", "alpha.txt")
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := Refresh(context.Background(), root, cachePath)
	if err != nil || !slices.Contains(files, first) {
		t.Fatalf("first refresh files=%v err=%v", files, err)
	}
	if cached, ok := Load(root, cachePath); !ok || !slices.Contains(cached, first) {
		t.Fatalf("cached files=%v ok=%t", cached, ok)
	}

	second := filepath.Join(root, "workspace", "beta.txt")
	if err := os.WriteFile(second, []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Some filesystems expose coarse directory timestamps.
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Dir(second), now, now); err != nil {
		t.Fatal(err)
	}
	files, err = Refresh(context.Background(), root, cachePath)
	if err != nil || !slices.Contains(files, first) || !slices.Contains(files, second) {
		t.Fatalf("incremental refresh files=%v err=%v", files, err)
	}
}

func TestRefreshExcludesHiddenAndDependencyTrees(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{".hidden/secret.txt", "node_modules/pkg/index.js", "vendor/pkg/source.go", "visible/report.txt"} {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := Refresh(context.Background(), root, filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "report.txt" {
		t.Fatalf("files=%v", files)
	}
}

func TestDarwinIndexExcludesSystemLibraryAndApplicationBundles(t *testing.T) {
	root := filepath.Join("fixtures", "home")
	if !skipDirectory("darwin", root, root, "Library") {
		t.Fatal("macOS user Library was not excluded")
	}
	if !skipDirectory("darwin", root, filepath.Join(root, "Downloads"), "Example.app") {
		t.Fatal("application bundle was not excluded")
	}
	if skipDirectory("linux", root, root, "Library") || skipDirectory("darwin", root, root, "Documents") {
		t.Fatal("ordinary user directory was excluded")
	}
}

func TestWindowsIndexExcludesProfileApplicationState(t *testing.T) {
	root := filepath.Join("fixtures", "windows-home")
	for _, name := range []string{"AppData", "Application Data", "Local Settings"} {
		if !skipDirectory("windows", root, root, name) {
			t.Fatalf("Windows profile state directory %q was not excluded", name)
		}
	}
	if skipDirectory("windows", root, root, "Documents") {
		t.Fatal("ordinary Windows user directory was excluded")
	}
	if skipDirectory("windows", root, filepath.Join(root, "Documents"), "AppData") {
		t.Fatal("nested user directory named AppData was excluded")
	}
}

func TestBackgroundRefreshIsConsumedWithoutStaleCache(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "index.json")
	file := filepath.Join(root, "current.txt")
	if err := os.WriteFile(file, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	RefreshInBackground(context.Background(), root, cachePath)
	files, err := Current(context.Background(), root, cachePath)
	if err != nil || !slices.Contains(files, file) {
		t.Fatalf("current files=%v err=%v", files, err)
	}
}

func TestBackgroundCacheWriteFailureIsObservedAndFreshRefreshRecovers(t *testing.T) {
	root := t.TempDir()
	cacheRoot := t.TempDir()
	blockedParent := filepath.Join(cacheRoot, "cache-parent")
	if err := os.WriteFile(blockedParent, []byte("private cache marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(blockedParent, "index.json")
	file := filepath.Join(root, "current.txt")
	if err := os.WriteFile(file, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}

	faults := make(chan errorreport.Fault, 2)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults <- fault
	})
	defer restore()
	ctx, cancel := context.WithCancel(supportref.WithContext(context.Background(), fileIndexSupportReference))
	cancel()
	RefreshInBackground(ctx, root, cachePath)
	waitRefreshReady(t, cachePath)

	var fault errorreport.Fault
	select {
	case fault = <-faults:
	case <-time.After(5 * time.Second):
		t.Fatal("background cache failure was not observed")
	}
	if fault.Component != "paperboat-cli" || fault.Operation != "send" || fault.Stage != "command" || fault.Code != "command_failed" || fault.Outcome != "failed" || fault.SupportReference != fileIndexSupportReference || !slices.Contains(fault.ErrorChain, "refreshFailure") || !slices.Contains(fault.ErrorChain, "PathError") {
		t.Fatalf("background cache fault lost its safe classification, cause, or reference: %+v", fault)
	}

	_, err := Current(supportref.WithContext(context.Background(), fileIndexSupportReference), root, cachePath)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || err.Error() != "file index refresh failed" || strings.Contains(err.Error(), cacheRoot) {
		t.Fatalf("foreground refresh did not retain its original cause behind safe text: %v", err)
	}
	if err := os.Remove(blockedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blockedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := Current(supportref.WithContext(context.Background(), fileIndexSupportReference), root, cachePath)
	if err != nil || !slices.Contains(files, file) {
		t.Fatalf("fresh cache refresh did not recover: files=%v err=%v", files, err)
	}
	select {
	case duplicate := <-faults:
		t.Fatalf("one background refresh emitted multiple observations: %+v", duplicate)
	default:
	}
}

func TestForegroundCacheWriteFailureReturnsCauseWithoutBackgroundObservation(t *testing.T) {
	root := t.TempDir()
	cacheRoot := t.TempDir()
	blockedParent := filepath.Join(cacheRoot, "cache-parent")
	if err := os.WriteFile(blockedParent, []byte("private cache marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(blockedParent, "index.json")
	faults := make(chan errorreport.Fault, 1)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults <- fault
	})
	defer restore()
	ctx := supportref.WithContext(context.Background(), fileIndexSupportReference)
	if _, err := Current(ctx, root, cachePath); err == nil {
		t.Fatal("foreground refresh unexpectedly succeeded")
	} else {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || err.Error() != "file index refresh failed" || strings.Contains(err.Error(), cacheRoot) {
			t.Fatalf("foreground refresh did not return its original cause behind safe text: %v", err)
		}
	}
	select {
	case fault := <-faults:
		t.Fatalf("foreground-owned refresh was additionally observed in the background: %+v", fault)
	default:
	}
	if err := os.Remove(blockedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blockedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Current(ctx, root, cachePath); err != nil {
		t.Fatalf("foreground refresh did not recover: %v", err)
	}
	select {
	case fault := <-faults:
		t.Fatalf("foreground recovery emitted a background observation: %+v", fault)
	default:
	}
}

func waitRefreshReady(t *testing.T, cachePath string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !RefreshReady(cachePath) {
		select {
		case <-deadline.C:
			t.Fatal("background refresh did not finish")
		case <-ticker.C:
		}
	}
}
