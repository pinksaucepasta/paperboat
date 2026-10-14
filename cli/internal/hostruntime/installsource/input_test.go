package installsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStageInputCopiesRelativeLocalFileAndBindsStagedBytes(t *testing.T) {
	root := t.TempDir()
	inputDir := filepath.Join(root, "source")
	stageDir := filepath.Join(root, "stage")
	if err := os.Mkdir(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stageDir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stageDir, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(inputDir, "release.pb-update")
	want := []byte("approved update bytes")
	if err := os.WriteFile(inputPath, want, 0600); err != nil {
		t.Fatal(err)
	}

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeInput, err := filepath.Rel(workingDir, inputPath)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := StageInput(context.Background(), relativeInput, stageDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(staged.Path) || !strings.HasSuffix(staged.Path, ".pb-update") {
		t.Fatalf("staged path=%q", staged.Path)
	}
	got, err := os.ReadFile(staged.Path)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(want)
	if string(got) != string(want) || staged.Length != int64(len(want)) || staged.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("staged bytes or identity differ: length=%d digest=%q", staged.Length, staged.SHA256)
	}
	info, err := os.Stat(staged.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("staged file mode=%#o", info.Mode().Perm())
	}
	if markerBytes, err := os.ReadFile(marker); err != nil || string(markerBytes) != "keep" {
		t.Fatalf("unrelated staged-directory file changed: %q, %v", markerBytes, err)
	}

	got[len(got)-1] ^= 1
	if err := os.WriteFile(staged.Path, got, 0600); err != nil {
		t.Fatal(err)
	}
	tamperedDigest := sha256.Sum256(got)
	if hex.EncodeToString(tamperedDigest[:]) == staged.SHA256 {
		t.Fatal("staged digest did not distinguish later byte tampering")
	}
}

func TestStageInputRejectsSymlinksAndOversizedLocalFiles(t *testing.T) {
	root := t.TempDir()
	stageDir := filepath.Join(root, "stage")
	if err := os.Mkdir(stageDir, 0700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "regular.pkg")
	if err := os.WriteFile(regular, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.pkg")
	if err := os.Symlink(regular, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := StageInput(context.Background(), link, stageDir, nil); !errors.Is(err, ErrInput) {
		t.Fatalf("symlink accepted: %v", err)
	}

	oversized := filepath.Join(root, "oversized.exe")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxInputBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := StageInput(context.Background(), oversized, stageDir, nil); !errors.Is(err, ErrInput) {
		t.Fatalf("oversized sparse file accepted: %v", err)
	}
	assertOnlyFiles(t, stageDir)
}

func TestStageInputDownloadsHTTPSWithoutAmbientCredentials(t *testing.T) {
	const payload = "downloaded package bytes"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "token=private-value" {
			t.Errorf("query was not preserved: %q", r.URL.RawQuery)
		}
		for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
			if got := r.Header.Get(header); got != "" {
				t.Errorf("unexpected %s header", header)
			}
		}
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()

	stageDir := t.TempDir()
	staged, err := StageInput(context.Background(), server.URL+"/download.pkg?token=private-value", stageDir, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(staged.Path, ".pkg") || staged.Length != int64(len(payload)) {
		t.Fatalf("staged identity=%+v", staged)
	}
	got, err := os.ReadFile(staged.Path)
	if err != nil || string(got) != payload {
		t.Fatalf("downloaded bytes=%q, err=%v", got, err)
	}
}

func TestStageInputRejectsUnsafeURLsAndDoesNotExposeThem(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	stageDir := t.TempDir()
	secretURL := server.URL + "/denied?token=secret-value"
	_, err := StageInput(context.Background(), secretURL, stageDir, server.Client())
	if !errors.Is(err, ErrInput) || strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unsafe status error=%v", err)
	}
	for _, input := range []string{
		"http://example.invalid/input.pkg",
		"https://user:password@example.invalid/input.pkg",
		"https://example.invalid/input.pkg#fragment",
	} {
		if _, err := StageInput(context.Background(), input, stageDir, nil); !errors.Is(err, ErrInput) {
			t.Errorf("unsafe URL accepted: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("unsafe URLs reached the server: requests=%d", requests.Load())
	}
	assertOnlyFiles(t, stageDir)
}

func TestStageInputRejectsHTTPDowngradeAndTooManyRedirects(t *testing.T) {
	var downgradeRequests atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downgradeRequests.Add(1)
		_, _ = io.WriteString(w, "must not be fetched")
	}))
	defer plain.Close()

	downloader := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/target?token=secret", http.StatusFound)
	}))
	defer downloader.Close()
	stageDir := t.TempDir()
	inputURL := downloader.URL + "/start?token=secret"
	_, err := StageInput(context.Background(), inputURL, stageDir, downloader.Client())
	if !errors.Is(err, ErrInput) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), inputURL) {
		t.Fatalf("downgrade redirect error=%v", err)
	}
	if downgradeRequests.Load() != 0 {
		t.Fatal("HTTP redirect target received the download")
	}
	assertOnlyFiles(t, stageDir)

	var hops atomic.Int32
	chain := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		var hop int
		if _, scanErr := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/hop/"), "%d", &hop); scanErr != nil {
			t.Errorf("invalid redirect path: %q", r.URL.Path)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d?token=secret", hop+1), http.StatusFound)
	}))
	defer chain.Close()
	_, err = StageInput(context.Background(), chain.URL+"/hop/0?token=secret", stageDir, chain.Client())
	if !errors.Is(err, ErrInput) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("redirect-limit error=%v", err)
	}
	if hops.Load() != maxInputRedirects+1 {
		t.Fatalf("redirect request count=%d, want %d", hops.Load(), maxInputRedirects+1)
	}
	assertOnlyFiles(t, stageDir)
}

func TestStageInputHonorsCancellationAndRemovesPartialFiles(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	stageDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := StageInput(ctx, server.URL+"/input.pkg?token=secret", stageDir, server.Client())
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not begin the response")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrInput) || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("cancel result=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("staging did not stop after cancellation")
	}
	assertOnlyFiles(t, stageDir)
}

func TestStageInputRejectsTruncatedResponseAndContentLengthOverflow(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/truncated":
			w.Header().Set("Content-Length", "20")
			_, _ = io.WriteString(w, "short")
		case "/overflow":
			w.Header().Set("Content-Length", fmt.Sprint(MaxInputBytes+1))
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	stageDir := t.TempDir()
	_, err := StageInput(context.Background(), server.URL+"/truncated", stageDir, server.Client())
	if !errors.Is(err, ErrInput) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated response result=%v", err)
	}
	assertOnlyFiles(t, stageDir)

	_, err = StageInput(context.Background(), server.URL+"/overflow?token=secret", stageDir, server.Client())
	if !errors.Is(err, ErrInput) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("oversized response result=%v", err)
	}
	assertOnlyFiles(t, stageDir)
}

func TestInspectBuildUsesCallerVersionAndCandidateProbesTrimpathBinary(t *testing.T) {
	root := t.TempDir()
	binary := buildPaperboatFixture(t, root, "pb", runtime.GOOS, runtime.GOARCH)
	marker := filepath.Join(root, "executed")
	t.Setenv("PB_INSPECT_MARKER", marker)

	source, err := InspectBuild(binary, "signed-index-version")
	if err != nil {
		t.Fatal(err)
	}
	if source.Version != "signed-index-version" || source.Distribution != Custom || source.AutomaticUpdates {
		t.Fatalf("passive inspection source=%+v", source)
	}
	if source.Platform != runtime.GOOS || source.Architecture != runtime.GOARCH {
		t.Fatalf("inspected target=%s/%s, want %s/%s", source.Platform, source.Architecture, runtime.GOOS, runtime.GOARCH)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive build inspection executed candidate: stat marker err=%v", err)
	}

	candidate, err := InspectCandidate(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Version != "fixture-version" || candidate.Distribution != Custom || candidate.AutomaticUpdates {
		t.Fatalf("candidate source=%+v", candidate)
	}
	if markerBytes, err := os.ReadFile(marker); err != nil || string(markerBytes) != "executed" {
		t.Fatalf("candidate probe marker=%q, err=%v", markerBytes, err)
	}

	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBuild(path, "signed-index-version"); !errors.Is(err, ErrInput) {
		t.Fatalf("non-Paperboat Go executable accepted: %v", err)
	}

	textFile := filepath.Join(root, "not-a-go-binary")
	if err := os.WriteFile(textFile, []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBuild(textFile, "signed-index-version"); err == nil {
		t.Fatal("non-Go file accepted as a Paperboat executable")
	}
}

func TestInspectBuildRejectsPaperboatExecutableForAnotherPlatform(t *testing.T) {
	wrongPlatform := "windows"
	if runtime.GOOS == "windows" {
		wrongPlatform = "linux"
	}
	binary := buildPaperboatFixture(t, t.TempDir(), "pb-wrong-target", wrongPlatform, runtime.GOARCH)
	if _, err := InspectBuild(binary, "signed-index-version"); err == nil {
		t.Fatal("Paperboat executable for another platform was accepted")
	}
}

func TestInspectCandidateRejectsMalformedOversizedAndCanceledProbes(t *testing.T) {
	binary := buildPaperboatFixture(t, t.TempDir(), "pb", runtime.GOOS, runtime.GOARCH)
	for _, mode := range []string{"malformed", "duplicate", "trailing", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PB_PROBE_MODE", mode)
			if _, err := InspectCandidate(context.Background(), binary); !errors.Is(err, ErrInput) {
				t.Fatalf("invalid probe output accepted: %v", err)
			}
		})
	}
	t.Setenv("PB_PROBE_MODE", "timeout")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := InspectCandidate(ctx, binary); !errors.Is(err, ErrInput) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled probe error=%v", err)
	}
}

func buildPaperboatFixture(t *testing.T, root, name, goos, goarch string) string {
	t.Helper()
	for _, directory := range []string{"cmd/pb", "internal/buildinfo"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture("go.mod", "module github.com/pinksaucepasta/paperboat\n\ngo 1.27.1\n")
	writeFixture("internal/buildinfo/buildinfo.go", "package buildinfo\nvar Version = \"dev\"\n")
	writeFixture("cmd/pb/main.go", `package main
import (
	"fmt"
	"os"
	"strings"
	"time"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
)
func main() {
	if marker := os.Getenv("PB_INSPECT_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("executed"), 0600)
	}
	switch os.Getenv("PB_PROBE_MODE") {
	case "timeout":
		time.Sleep(10 * time.Second)
		return
	case "malformed":
		fmt.Print("{")
		return
	case "duplicate":
		fmt.Print("{\"schema_version\":\"1.0\",\"schema_version\":\"1.0\",\"ok\":true,\"data\":{\"command\":\"pb\",\"version\":\"fixture-version\"}}")
		return
	case "trailing":
		fmt.Print("{\"schema_version\":\"1.0\",\"ok\":true,\"data\":{\"command\":\"pb\",\"version\":\"fixture-version\"}}{}")
		return
	case "oversized":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 4097)))
		return
	}
	fmt.Printf("{\"schema_version\":\"1.0\",\"ok\":true,\"data\":{\"command\":\"pb\",\"version\":%q}}", buildinfo.Version)
}
`)
	if goos == "windows" && filepath.Ext(name) == "" {
		name += ".exe"
	}
	binary := filepath.Join(root, name)
	ldflags := "-X github.com/pinksaucepasta/paperboat/internal/buildinfo.Version=fixture-version"
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "-ldflags", ldflags, "./cmd/pb")
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off", "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s/%s inspection fixture: %v: %s", goos, goarch, err, output)
	}
	return binary
}

func assertOnlyFiles(t *testing.T, directory string, allowed ...string) {
	t.Helper()
	want := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		want[name] = struct{}{}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			t.Errorf("unexpected leftover staged file %q", entry.Name())
		}
	}
}
