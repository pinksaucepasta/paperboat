// Package selfupdate lets pbh update itself when an operator presses Update: the self-host bundle is verified
// against the same signed TUF metadata pb's own updates use (the embedded root in shared/tufroot) before anything
// on the host changes.
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := config.New(strings.TrimRight(repositoryURL, "/")+"/metadata/", root)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignature, err)
	}
	cfg.LocalMetadataDir, cfg.LocalTargetsDir = filepath.Join(cacheDir, "metadata"), filepath.Join(cacheDir, "targets")
	cfg.PrefixTargetsWithHash = true
	cfg.MaxRootRotations, cfg.RootMaxLength, cfg.TimestampMaxLength, cfg.SnapshotMaxLength, cfg.TargetsMaxLength = 32, 128<<10, 64<<10, 256<<10, 512<<10
	for _, dir := range []string{cfg.LocalMetadataDir, cfg.LocalTargetsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if client != nil {
		if err := cfg.SetDefaultFetcherHTTPClient(client); err != nil {
			return err
		}
	}
	tuf, err := updater.New(cfg)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignature, err)
	}
	if err := tuf.Refresh(); err != nil {
		return fmt.Errorf("%w: refresh signed metadata: %w", ErrSignature, err)
	}
	info, err := tuf.GetTargetInfo(TargetName(version, arch))
	if err != nil || info == nil {
		return fmt.Errorf("%w: %s is not a signed target", ErrSignature, TargetName(version, arch))
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
