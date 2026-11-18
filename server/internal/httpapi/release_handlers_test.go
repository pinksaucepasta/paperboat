package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseDownloadsAreNotCutOffByAPIRequestTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := timeout(time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("complete"))
	}))

	for _, path := range []string{"/tuf/targets/hash.pb-linux-amd64", "/helper-releases/tuf/targets/hash.pb-linux-amd64"} {
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			close(done)
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("release handler did not start for %s", path)
		}
		release <- struct{}{}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("release handler did not finish for %s", path)
		}
		if recorder.Code != http.StatusOK || recorder.Body.String() != "complete" {
			t.Fatalf("%s response = %d %q", path, recorder.Code, recorder.Body.String())
		}
	}
}

// First install no longer touches this server: the install scripts live in the repository and GitHub, so the server serves
// none of them, even when stale script files sit in the release directory. Only signed TUF metadata is served.
func TestReleaseEndpointsServeTUFMetadataAndNoInstallScripts(t *testing.T) {
	directory := t.TempDir()
	for path, body := range map[string]string{
		"install": "#!/bin/sh\necho paperboat\n", "windows": "Write-Output paperboat-windows\n",
		"selfhost/install": "#!/bin/sh\necho selfhost\n", "selfhost/versions/2026.11.01.0/paperboat-selfhost-linux-amd64.tar.gz": "package",
		"tuf/metadata/timestamp.json": `{"signed":{"version":1}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := NewReleaseFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Options{OverrideHandler: nil, ReleaseFiles: files})
	for _, path := range []string{
		"/install", "/install.ps1", "/windows", "/recipe", "/recipe.ps1", "/selfhost/install", "/selfhost/versions/2026.11.01.0/paperboat-selfhost-linux-amd64.tar.gz",
		"/install?p=4J7M9Q2V8X4N6P5R1T0W8Y2ZAB", "/install.ps1?p=4J7M9Q2V8X4N6P5R1T0W8Y2ZAB", "/recipe?t=pbr_x", "/current.json",
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d %q; the server must not serve install scripts", path, response.Code, response.Body.String())
		}
	}
	// The release file handler itself refuses everything but TUF metadata.
	for _, path := range []string{"/install", "/windows", "/selfhost/install"} {
		response := httptest.NewRecorder()
		files.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("release files served %s: %d", path, response.Code)
		}
	}

	timestamp := httptest.NewRecorder()
	router.ServeHTTP(timestamp, httptest.NewRequest(http.MethodGet, "/tuf/metadata/timestamp.json", nil))
	if timestamp.Code != http.StatusOK || !strings.Contains(timestamp.Body.String(), `"version":1`) {
		t.Fatalf("timestamp response = %d %q", timestamp.Code, timestamp.Body.String())
	}
	if got := timestamp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("timestamp cache control = %q", got)
	}
}

func TestNewReleaseFilesRejectsRelativeAndSymlinkDirectories(t *testing.T) {
	if _, err := NewReleaseFiles("relative"); err == nil {
		t.Fatal("expected relative directory rejection")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "release-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReleaseFiles(link); err == nil {
		t.Fatal("expected symlink directory rejection")
	}
}

func TestReleaseFilesObservesReplacedReleaseDirectory(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	next := filepath.Join(root, "next")
	for path, version := range map[string]string{current: "2026.08.22.21", next: "2026.08.22.22"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"signed":{"version":"` + version + `"}}`)
		if err := os.MkdirAll(filepath.Join(path, "tuf", "metadata"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "tuf", "metadata", "timestamp.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := NewReleaseFiles(current)
	if err != nil {
		t.Fatal(err)
	}
	assertVersion := func(want string) {
		t.Helper()
		response := httptest.NewRecorder()
		files.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tuf/metadata/timestamp.json", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("release response status=%d body=%q, want version %q", response.Code, response.Body.String(), want)
		}
	}
	assertVersion("2026.08.22.21")
	if err := os.Rename(current, filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, current); err != nil {
		t.Fatal(err)
	}
	assertVersion("2026.08.22.22")
}

func TestTUFRepositoryDoesNotExposePrivateOrUnrelatedFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "tuf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "tuf", ".signing-state.json"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "tuf", "targets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "tuf", "targets", strings.Repeat("a", 64)+".pb-linux-amd64"), []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewReleaseFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/tuf/.signing-state.json", "/tuf/unrelated.txt", "/tuf/targets/" + strings.Repeat("a", 64) + ".pb-linux-amd64"} {
		recorder := httptest.NewRecorder()
		tufRepository("/tuf", files).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", path, recorder.Code)
		}
	}
}

func TestReleaseFilesRejectNestedSymlink(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "tuf", "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "tuf", "metadata", "timestamp.json")); err != nil {
		t.Fatal(err)
	}
	files, err := NewReleaseFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	files.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tuf/metadata/timestamp.json", nil))
	if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("symlink response = %d %q", recorder.Code, recorder.Body.String())
	}
}
