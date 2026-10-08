package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
)

// TestStagedTUFRepository runs the same signed release-index and component
// consumers used by a native runtime, but serves the freshly signed repository
// from the workflow's isolated staging directory. This must run before that
// directory is activated on the public origin.
func TestStagedTUFRepository(t *testing.T) {
	releaseRoot, githubRoot, err := stagedReleaseVerificationDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if releaseRoot == "" {
		t.Skip("staged release verification is not configured")
	}
	candidateVersion := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_TUF_VERSION"))
	if candidateVersion == "" {
		t.Fatal("PAPERBOAT_TEST_TUF_VERSION is not set")
	}
	if !cleanAbsoluteDirectory(releaseRoot) || !cleanAbsoluteDirectory(githubRoot) {
		t.Fatal("staged release paths must be absolute, clean directories")
	}
	assertStagedGitHubAssets(t, githubRoot)
	pins := readStagedProductPins(t, filepath.Join(releaseRoot, "tuf", "metadata", "targets.json"))
	candidatePresent := false
	for _, pin := range pins {
		if pin.Version == candidateVersion {
			candidatePresent = true
			break
		}
	}
	if !candidatePresent {
		t.Fatalf("staged product targets do not include candidate version %q", candidateVersion)
	}
	installers := make(map[string][]byte, 2)
	for _, name := range []string{"install", "windows"} {
		body := readStagedRegularFile(t, filepath.Join(releaseRoot, name))
		if bytes.Contains(body, []byte("pb-bootstrap")) || bytes.Contains(body, []byte("@PAPERBOAT_BOOTSTRAP_")) || regexp.MustCompile(`@PAPERBOAT_[A-Z0-9_]+@`).Match(body) {
			t.Fatalf("staged %s contains a helper asset reference or unresolved product pin", name)
		}
		installers[name] = body
	}
	for _, target := range []struct {
		platform, architecture, installer string
		powershell                        bool
	}{
		{"linux", "amd64", "install", false},
		{"linux", "arm64", "install", false},
		{"darwin", "arm64", "install", false},
		{"windows", "amd64", "windows", true},
		{"windows", "arm64", "windows", true},
	} {
		assetName := releaseindex.AssetName(target.platform, target.architecture)
		pin := pins[assetName]
		var expected []string
		if target.powershell {
			expected = []string{
				"$productVersion = '" + pin.Version + "'",
				"$productUrl = '" + pin.URL + "'",
				"$productSha = '" + pin.SHA256 + "'",
				fmt.Sprintf("$productLength = '%d'", pin.Length),
			}
		} else {
			expected = []string{
				"product_version='" + pin.Version + "'",
				"product_url='" + pin.URL + "'",
				"product_sha='" + pin.SHA256 + "'",
				fmt.Sprintf("product_length='%d'", pin.Length),
			}
		}
		for _, value := range expected {
			if !bytes.Contains(installers[target.installer], []byte(value)) {
				t.Fatalf("staged %s is missing %s pin for %s", target.installer, value, assetName)
			}
		}
	}

	repositoryRoot := filepath.Join(releaseRoot, "tuf")
	server := stagedTUFServer(t, repositoryRoot)
	defer server.Close()
	client := server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, target := range []struct{ platform, architecture string }{
		{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"},
	} {
		t.Run(target.platform+"-"+target.architecture, func(t *testing.T) {
			stateRoot := t.TempDir()
			productVersion := pins[releaseindex.AssetName(target.platform, target.architecture)].Version

			now := time.Now().UTC()
			index, err := fetchVerifiedReleaseIndex(ctx, server.URL, filepath.Join(stateRoot, "index"), client, now, target.platform, target.architecture)
			if err != nil {
				t.Fatal(err)
			}
			if index.Version != productVersion {
				t.Fatalf("staged release index version=%q, want %q", index.Version, productVersion)
			}
			targetInfo, ok := index.Component("pb")
			if !ok {
				t.Fatal("staged release index has no pb component")
			}
			path, err := fetchVerifiedReleaseComponent(ctx, server.URL, filepath.Join(stateRoot, "pb"), index, "pb", client, now, target.platform, target.architecture)
			if err != nil {
				t.Fatalf("pb component: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(body)
			if targetInfo.Length != int64(len(body)) || targetInfo.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatal("pb component does not match its signed target")
			}
			githubComponent := readStagedRegularFile(t, filepath.Join(githubRoot, targetInfo.TargetPath))
			if !bytes.Equal(body, githubComponent) {
				t.Fatal("pb component differs from the immutable GitHub asset")
			}
		})
	}
}

type stagedProductPin struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Length  int64  `json:"length"`
}

func readStagedProductPins(t *testing.T, path string) map[string]stagedProductPin {
	t.Helper()
	var document struct {
		Signed struct {
			Targets map[string]struct {
				Custom *stagedProductPin `json:"custom"`
			} `json:"targets"`
		} `json:"signed"`
	}
	if err := json.Unmarshal(readStagedRegularFile(t, path), &document); err != nil {
		t.Fatalf("decode staged product target metadata: %v", err)
	}
	assets := canonicalProductAssetNames()
	if len(document.Signed.Targets) != len(assets) {
		t.Fatalf("staged product target count=%d, want %d canonical products", len(document.Signed.Targets), len(assets))
	}
	pins := make(map[string]stagedProductPin, len(assets))
	for _, name := range assets {
		target, ok := document.Signed.Targets[name]
		if !ok || target.Custom == nil {
			t.Fatalf("staged product target %s is missing signed custom metadata", name)
		}
		pin := *target.Custom
		decodedDigest, err := hex.DecodeString(pin.SHA256)
		if err != nil || len(decodedDigest) != sha256.Size || pin.Version == "" || pin.URL == "" || pin.Length <= 0 {
			t.Fatalf("staged product target %s has incomplete pins", name)
		}
		pins[name] = pin
	}
	return pins
}

func assertStagedGitHubAssets(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read staged GitHub release assets: %v", err)
	}
	expected := make(map[string]struct{}, 5)
	for _, name := range canonicalProductAssetNames() {
		expected[name] = struct{}{}
	}
	if len(entries) != len(expected) {
		t.Fatalf("staged GitHub release asset count=%d, want exactly %d canonical products", len(entries), len(expected))
	}
	for _, entry := range entries {
		if _, ok := expected[entry.Name()]; !ok {
			t.Fatalf("unexpected staged GitHub release asset %s", entry.Name())
		}
	}
}

func canonicalProductAssetNames() []string {
	return []string{
		releaseindex.AssetName("windows", "amd64"),
		releaseindex.AssetName("windows", "arm64"),
		releaseindex.AssetName("linux", "amd64"),
		releaseindex.AssetName("linux", "arm64"),
		releaseindex.AssetName("darwin", "arm64"),
	}
}

func TestStagedReleaseVerificationCannotSkipWhenRequired(t *testing.T) {
	t.Setenv("PAPERBOAT_TEST_REQUIRE_STAGED", "1")
	t.Setenv("PAPERBOAT_TEST_RELEASE_DIRECTORY", "")
	t.Setenv("PAPERBOAT_TEST_GITHUB_RELEASE_DIRECTORY", "")
	if _, _, err := stagedReleaseVerificationDirectories(); err == nil {
		t.Fatal("required staged verification accepted missing directories")
	}
}

func TestStagedReleaseVerificationRejectsInvalidRequireFlag(t *testing.T) {
	t.Setenv("PAPERBOAT_TEST_REQUIRE_STAGED", "true")
	t.Setenv("PAPERBOAT_TEST_RELEASE_DIRECTORY", "")
	t.Setenv("PAPERBOAT_TEST_GITHUB_RELEASE_DIRECTORY", "")
	if _, _, err := stagedReleaseVerificationDirectories(); err == nil {
		t.Fatal("invalid staged verification gate was accepted")
	}
}

func stagedReleaseVerificationDirectories() (string, string, error) {
	releaseRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_RELEASE_DIRECTORY"))
	githubRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_GITHUB_RELEASE_DIRECTORY"))
	require := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_REQUIRE_STAGED"))
	if require != "" && require != "1" {
		return "", "", fmt.Errorf("PAPERBOAT_TEST_REQUIRE_STAGED must be exactly 1 when set, got %q", require)
	}
	if releaseRoot == "" || githubRoot == "" {
		if require == "1" {
			return "", "", errors.New("required staged release or downloaded GitHub release directory is not set")
		}
		return "", "", nil
	}
	return releaseRoot, githubRoot, nil
}

func cleanAbsoluteDirectory(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func readStagedRegularFile(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("required staged file is unavailable: %s", filepath.Base(path))
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertStagedFileEquals(t *testing.T, staged, immutable string) {
	t.Helper()
	if !bytes.Equal(readStagedRegularFile(t, staged), readStagedRegularFile(t, immutable)) {
		t.Fatalf("staged %s differs from immutable GitHub asset %s", filepath.Base(staged), filepath.Base(immutable))
	}
}

func stagedTUFServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(request.URL.Path)), "/")
		if relative == "" || strings.Contains(relative, "..") {
			http.NotFound(writer, request)
			return
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			http.NotFound(writer, request)
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		defer file.Close()
		http.ServeContent(writer, request, filepath.Base(path), info.ModTime(), file)
	}))
}
