// Package selfupdate lets pbh update itself when an operator presses Update: the self-host bundle is verified
// against the same signed TUF metadata pb's own updates use (the embedded root in shared/tufroot) before anything
// on the host changes.
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/releaseindex"
	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"
)

// ErrSignature: the bundle is not the one the signed release metadata names (or the metadata does not verify).
var ErrSignature = errors.New("the self-host release does not match its signed release metadata")

// TargetName is the TUF target of the self-host bundle for one version and Linux architecture.
func TargetName(version, arch string) string {
	return "selfhost/" + version + "/paperboat-selfhost-linux-" + arch + ".tar.gz"
}

// Verify checks bundle (a local file) against the TUF target for version and arch in the repository at
// repositoryURL (…/tuf), whose metadata is trusted from root. cacheDir keeps the client's metadata between runs.
func Verify(ctx context.Context, repositoryURL string, root []byte, cacheDir, version, arch, bundle string, client *http.Client) error {
	info, _, err := Target(ctx, repositoryURL, root, cacheDir, version, arch, client)
	if err != nil {
		return err
	}
	file, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	length, err := io.Copy(digest, io.LimitReader(file, info.Length+1))
	if err != nil {
		return err
	}
	if length != info.Length || !bytes.Equal(digest.Sum(nil), info.Hashes["sha256"]) {
		return fmt.Errorf("%w: length or sha256 differs", ErrSignature)
	}
	return nil
}

// refreshed is the TUF client with fresh signed metadata from repositoryURL, trusted from root.
func refreshed(ctx context.Context, repositoryURL string, root []byte, cacheDir string, client *http.Client) (*updater.Updater, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := config.New(strings.TrimRight(repositoryURL, "/")+"/metadata/", root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	cfg.LocalMetadataDir, cfg.LocalTargetsDir = filepath.Join(cacheDir, "metadata"), filepath.Join(cacheDir, "targets")
	cfg.PrefixTargetsWithHash = true
	cfg.MaxRootRotations, cfg.RootMaxLength, cfg.TimestampMaxLength, cfg.SnapshotMaxLength, cfg.TargetsMaxLength = 32, 128<<10, 64<<10, 256<<10, 512<<10
	for _, dir := range []string{cfg.LocalMetadataDir, cfg.LocalTargetsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if client != nil {
		if err := cfg.SetDefaultFetcherHTTPClient(client); err != nil {
			return nil, err
		}
	}
	tuf, err := updater.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	if err := tuf.Refresh(); err != nil {
		return nil, fmt.Errorf("%w: refresh signed metadata: %w", ErrSignature, err)
	}
	return tuf, nil
}

// Target refreshes the signed metadata and returns the bundle's target and the download URL signed into it
// (the GitHub release that carries the bundle). The URL is only where the bytes are; Verify checks them.
func Target(ctx context.Context, repositoryURL string, root []byte, cacheDir, version, arch string, client *http.Client) (*metadata.TargetFiles, string, error) {
	tuf, err := refreshed(ctx, repositoryURL, root, cacheDir, client)
	if err != nil {
		return nil, "", err
	}
	info, err := tuf.GetTargetInfo(TargetName(version, arch))
	if err != nil || info == nil {
		return nil, "", fmt.Errorf("%w: %s is not a signed target", ErrSignature, TargetName(version, arch))
	}
	var custom struct {
		Repository string `json:"repository"`
		URL        string `json:"url"`
	}
	if info.Custom == nil || json.Unmarshal(*info.Custom, &custom) != nil || !releaseindex.ValidDownloadURL(custom.URL, custom.Repository, "selfhost-"+version, path.Base(TargetName(version, arch))) {
		return nil, "", fmt.Errorf("%w: %s names no valid release download URL", ErrSignature, TargetName(version, arch))
	}
	return info, custom.URL, nil
}

// Latest is the newest self-host version the signed metadata carries a bundle for on arch.
func Latest(ctx context.Context, repositoryURL string, root []byte, cacheDir, arch string, client *http.Client) (string, error) {
	tuf, err := refreshed(ctx, repositoryURL, root, cacheDir, client)
	if err != nil {
		return "", err
	}
	latest := ""
	for name := range tuf.GetTopLevelTargets() {
		version := strings.TrimPrefix(name, "selfhost/")
		version = strings.TrimSuffix(version, "/paperboat-selfhost-linux-"+arch+".tar.gz")
		if version == name || strings.Contains(version, "/") || len(strings.Split(version, ".")) != 4 {
			continue
		}
		if latest == "" || Newer(version, latest) {
			latest = version
		}
	}
	if latest == "" {
		return "", fmt.Errorf("%w: no self-host release is signed for linux-%s", ErrSignature, arch)
	}
	return latest, nil
}

// Newer reports whether release version a (YYYY.MM.DD.N) is newer than b. A version that is not a release
// version (a development build) is never newer and never older-than anything: callers decide.
func Newer(a, b string) bool {
	x, y := versionParts(a), versionParts(b)
	if x == nil || y == nil {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func versionParts(version string) []int {
	fields := strings.Split(version, ".")
	if len(fields) != 4 {
		return nil
	}
	parts := make([]int, 4)
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil
		}
		parts[i] = n
	}
	return parts
}
