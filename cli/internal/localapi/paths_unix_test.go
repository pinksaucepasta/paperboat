//go:build darwin || linux

package localapi

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolvePathsUsesCanonicalStateAndSafeRuntimeFallback(t *testing.T) {
	home := localAPITestDir(t)
	environment := map[string]string{}
	if runtime.GOOS == "linux" {
		environment["XDG_STATE_HOME"] = filepath.Join(home, "state")
		environment["XDG_RUNTIME_DIR"] = filepath.Join(home, "missing-runtime")
	} else {
		environment["TMPDIR"] = filepath.Join(home, "missing-runtime")
	}
	paths, err := ResolvePaths(func(key string) string { return environment[key] }, home, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	wantState := filepath.Join(home, "Library", "Application Support", "Paperboat", "state")
	if runtime.GOOS == "linux" {
		wantState = filepath.Join(environment["XDG_STATE_HOME"], "paperboat")
	}
	if paths.StateRoot != wantState || paths.RuntimeRoot != filepath.Join(wantState, "run") || paths.SocketPath != filepath.Join(paths.RuntimeRoot, "local-api.sock") || paths.LockPath != filepath.Join(wantState, "daemon.lock") {
		t.Fatalf("paths=%#v", paths)
	}
	for _, path := range []string{paths.StateRoot, paths.RuntimeRoot} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 || fileOwner(info) != os.Geteuid() {
			t.Fatalf("path=%s info=%#v err=%v", path, info, err)
		}
	}
}

func TestResolvePathsUsesSafeRuntimeAndRejectsRelativeOverrides(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux XDG runtime overrides")
	}
	home := localAPITestDir(t)
	runtimeRoot := filepath.Join(home, "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{}
	if runtime.GOOS == "linux" {
		environment["XDG_STATE_HOME"] = filepath.Join(home, "state")
		environment["XDG_RUNTIME_DIR"] = runtimeRoot
	} else {
		environment["TMPDIR"] = runtimeRoot
	}
	paths, err := ResolvePaths(func(key string) string { return environment[key] }, home, os.Geteuid())
	if err != nil || paths.RuntimeRoot != filepath.Join(runtimeRoot, "paperboat") {
		t.Fatalf("paths=%#v err=%v", paths, err)
	}
	if runtime.GOOS == "linux" {
		environment["XDG_STATE_HOME"] = "relative"
	} else {
		environment["TMPDIR"] = "relative"
	}
	if _, err := ResolvePaths(func(key string) string { return environment[key] }, home, os.Geteuid()); err == nil {
		t.Fatal("relative override was accepted")
	}
}

func TestDarwinSocketNamespaceIndependentOfTemporaryEnvironment(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin stable per-user socket namespace")
	}
	home := localAPITestDir(t)
	runtimeRoot := filepath.Join(home, "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "Application Support", "Paperboat", "state", "run")
	for _, temporary := range []string{"", runtimeRoot, runtimeRoot + string(os.PathSeparator), filepath.Join(home, "missing"), "relative"} {
		paths, err := ResolvePaths(func(key string) string {
			if key == "TMPDIR" {
				return temporary
			}
			return ""
		}, home, os.Geteuid())
		if err != nil || paths.RuntimeRoot != want || paths.SocketPath != filepath.Join(want, "local-api.sock") {
			t.Fatalf("TMPDIR=%q paths=%#v err=%v", temporary, paths, err)
		}
	}
}

func TestResolvePathsRejectsPermissiveOwnedRuntimeDirectory(t *testing.T) {
	home := localAPITestDir(t)
	runtimeRoot := filepath.Join(home, "runtime")
	if err := os.Mkdir(runtimeRoot, 0o733); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeRoot, 0o733); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{}
	if runtime.GOOS == "linux" {
		environment["XDG_STATE_HOME"] = filepath.Join(home, "state")
		environment["XDG_RUNTIME_DIR"] = runtimeRoot
	} else {
		environment["TMPDIR"] = runtimeRoot
	}
	paths, err := ResolvePaths(func(key string) string { return environment[key] }, home, os.Geteuid())
	if err != nil || paths.RuntimeRoot == filepath.Join(runtimeRoot, "paperboat") {
		t.Fatalf("paths=%#v err=%v", paths, err)
	}
}
