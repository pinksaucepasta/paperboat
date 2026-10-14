package bootstrap

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
	"github.com/theupdateframework/go-tuf/v2/metadata/trustedmetadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"
)

// VerifiedBundle contains authenticated release bytes. The caller owns the
// returned artifact and must remove it after the installation transaction.
type VerifiedBundle struct {
	Index        releaseindex.Index
	ArtifactPath string
}

const bundleArtifact = "artifact"
const maxBundleMetadata = 6 << 20

// rootFetcher preserves authenticated sequential root updates independently
// of later refresh failures. Subsequent verification resumes from this chain.
type rootFetcher struct {
	ctx       context.Context
	base      fetcher.Fetcher
	roots     *trustedmetadata.TrustedMetadata
	directory string
}

func (f *rootFetcher) DownloadFile(raw string, limit int64, timeout time.Duration) ([]byte, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	data, err := f.base.DownloadFile(raw, limit, timeout)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrArtifactMismatch
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(u.Path)
	if strings.HasSuffix(name, ".root.json") {
		if _, err = f.roots.UpdateRoot(data); err != nil {
			return nil, err
		}
		if err = saveRoot(filepath.Join(f.directory, name), data); err != nil {
			return nil, err
		}
	}
	return data, nil
}
func saveRoot(path string, data []byte) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1})
}
func refreshArtifactVerifier(client *updater.Updater, directory string) error {
	if err := client.Refresh(); err != nil {
		return err
	}
	for _, name := range []string{"root.json", "timestamp.json", "snapshot.json", "targets.json"} {
		path := filepath.Join(directory, "metadata", name)
		data, err := readBoundedRegular(path, 512<<10)
		if err != nil {
			return err
		}
		if err = saveRoot(path, data); err != nil {
			return err
		}
	}
	return nil
}
func rootState(ctx context.Context, directory string, root []byte, source fetcher.Fetcher, metadataURL string) (*trustedmetadata.TrustedMetadata, []byte, error) {
	state, err := trustedmetadata.New(root)
	if err != nil {
		return nil, nil, err
	}
	if err = secureDirectory(filepath.Join(directory, "roots")); err != nil {
		return nil, nil, err
	}
	// Protected stores from prior releases already hold accepted roots. Recover
	// their continuity from authenticated sequential proofs before updater.New
	// can rewrite root.json; never use the cached root as a new trust anchor.
	var cachedRoot *metadata.Metadata[metadata.RootType]
	cached, cachedErr := readBoundedRegular(filepath.Join(directory, "metadata", "root.json"), 128<<10)
	if cachedErr == nil {
		cachedRoot, err = metadata.Root().FromBytes(cached)
		if err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(cachedErr, os.ErrNotExist) {
		return nil, nil, cachedErr
	}
	acceptRoot := func(data []byte) error {
		if _, err := state.UpdateRoot(data); err != nil {
			return err
		}
		if cachedRoot != nil && cachedRoot.Signed.Version == state.Root.Signed.Version && !reflect.DeepEqual(cachedRoot.Signed, state.Root.Signed) {
			return ErrArtifactMismatch
		}
		return nil
	}
	for count := 0; count < 32; count++ {
		next := state.Root.Signed.Version + 1
		path := filepath.Join(directory, "roots", strconv.FormatInt(next, 10)+".root.json")
		data, err := readBoundedRegular(path, 128<<10)
		if errors.Is(err, os.ErrNotExist) {
			if cachedRoot == nil || next > cachedRoot.Signed.Version {
				break
			}
			if err = ctx.Err(); err != nil {
				return nil, nil, err
			}
			data, err = source.DownloadFile(metadataURL+strconv.FormatInt(next, 10)+".root.json", 128<<10, 30*time.Second)
			if err != nil {
				return nil, nil, err
			}
			if len(data) > 128<<10 {
				return nil, nil, ErrArtifactMismatch
			}
			if err = acceptRoot(data); err != nil {
				return nil, nil, err
			}
			if err = saveRoot(path, data); err != nil {
				return nil, nil, err
			}
			root = data
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if err = acceptRoot(data); err != nil {
			return nil, nil, err
		}
		root = data
	}
	if cachedRoot != nil {
		if cachedRoot.Signed.Version > state.Root.Signed.Version {
			return nil, nil, ErrArtifactMismatch
		}
		if cachedRoot.Signed.Version == state.Root.Signed.Version && !reflect.DeepEqual(cachedRoot.Signed, state.Root.Signed) {
			return nil, nil, ErrArtifactMismatch
		}
	}
	return state, root, nil
}
func newArtifactVerifier(ctx context.Context, repositoryURL, directory string, root []byte, httpClient *http.Client, source fetcher.Fetcher) (*updater.Updater, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := url.Parse(repositoryURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, ErrArtifactTarget
	}
	if err = secureDirectory(directory); err != nil {
		return nil, err
	}
	cfg, err := config.New(strings.TrimRight(repositoryURL, "/")+"/metadata/", root)
	if err != nil {
		return nil, err
	}
	cfg.LocalMetadataDir = filepath.Join(directory, "metadata")
	cfg.LocalTargetsDir = filepath.Join(directory, "targets")
	cfg.RemoteTargetsURL = strings.TrimRight(repositoryURL, "/") + "/targets/"
	cfg.PrefixTargetsWithHash = true
	cfg.MaxRootRotations, cfg.RootMaxLength, cfg.TimestampMaxLength, cfg.SnapshotMaxLength, cfg.TargetsMaxLength = 32, 128<<10, 64<<10, 256<<10, 512<<10
	if source == nil {
		if err = cfg.SetDefaultFetcherHTTPClient(secureArtifactHTTPClient(httpClient, parsedOrigin(repositoryURL))); err != nil {
			return nil, err
		}
		source = cfg.Fetcher
	}
	state, root, err := rootState(ctx, directory, root, source, cfg.RemoteMetadataURL)
	if err != nil {
		return nil, err
	}
	cfg.LocalTrustedRoot = root
	cfg.Fetcher = &rootFetcher{ctx: ctx, base: source, roots: state, directory: filepath.Join(directory, "roots")}
	return updater.New(cfg)
}
func readBoundedRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrArtifactMismatch
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrArtifactMismatch
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrArtifactMismatch
	}
	return b, nil
}

// ExportUpdateBundle writes an untrusted transport container around unchanged
// official bytes and their existing TUF proof. No new release asset is published.
// Metadata access is serialized within the authoritative state directory.
func ExportUpdateBundle(ctx context.Context, repositoryURL, stateDirectory, outputPath string, httpClient *http.Client, now time.Time) (releaseindex.Index, error) {
	return exportUpdateBundle(ctx, repositoryURL, stateDirectory, outputPath, httpClient, now, trustedRoot, runtime.GOOS, runtime.GOARCH)
}
func exportUpdateBundle(ctx context.Context, repositoryURL, stateDirectory, outputPath string, httpClient *http.Client, now time.Time, root []byte, platform, architecture string) (releaseindex.Index, error) {
	if !filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		return releaseindex.Index{}, ErrArtifactTarget
	}
	unlock, err := lockArtifactState(ctx, stateDirectory)
	if err != nil {
		return releaseindex.Index{}, err
	}
	defer unlock()
	client, err := newArtifactVerifier(ctx, repositoryURL, stateDirectory, root, httpClient, nil)
	if err != nil {
		return releaseindex.Index{}, err
	}
	if err = refreshArtifactVerifier(client, stateDirectory); err != nil {
		return releaseindex.Index{}, err
	}
	index, err := verifiedIndex(client, now, platform, architecture)
	if err != nil {
		return releaseindex.Index{}, err
	}
	proofs, err := bundleProofs(stateDirectory, root)
	if err != nil {
		return releaseindex.Index{}, err
	}
	unlock()
	target, _ := index.Component("pb")
	digest, _ := hex.DecodeString(target.SHA256)
	staging, err := os.MkdirTemp(stateDirectory, ".bundle-export-")
	if err != nil {
		return releaseindex.Index{}, err
	}
	defer os.RemoveAll(staging)
	artifact, err := downloadVerifiedGitHubAsset(ctx, secureArtifactHTTPClient(httpClient, parsedOrigin(repositoryURL)), target.DownloadURL, staging, target.AssetName, target.Length, digest)
	if err != nil {
		return releaseindex.Index{}, err
	}
	pending, err := os.CreateTemp(filepath.Dir(outputPath), ".pb-update-")
	if err != nil {
		return releaseindex.Index{}, err
	}
	defer os.Remove(pending.Name())
	defer pending.Close()
	tw := tar.NewWriter(pending)
	names := make([]string, 0, len(proofs))
	for name := range proofs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return releaseindex.Index{}, err
		}
		data := proofs[name]
		if err = tw.WriteHeader(&tar.Header{Name: "metadata/" + name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return releaseindex.Index{}, err
		}
		if _, err = tw.Write(data); err != nil {
			return releaseindex.Index{}, err
		}
	}
	if err = tw.WriteHeader(&tar.Header{Name: bundleArtifact, Mode: 0600, Size: target.Length, Typeflag: tar.TypeReg}); err != nil {
		return releaseindex.Index{}, err
	}
	f, err := os.Open(artifact)
	if err != nil {
		return releaseindex.Index{}, err
	}
	_, copyErr := io.Copy(tw, &contextReader{ctx: ctx, r: f})
	closeErr := f.Close()
	if copyErr != nil {
		return releaseindex.Index{}, copyErr
	}
	if closeErr != nil {
		return releaseindex.Index{}, closeErr
	}
	if err = tw.Close(); err != nil {
		return releaseindex.Index{}, err
	}
	if err = pending.Sync(); err != nil {
		return releaseindex.Index{}, err
	}
	if err = pending.Close(); err != nil {
		return releaseindex.Index{}, err
	}
	if err = ctx.Err(); err != nil {
		return releaseindex.Index{}, err
	}
	if err = os.Rename(pending.Name(), outputPath); err != nil {
		return releaseindex.Index{}, err
	}
	return index, nil
}
func bundleProofs(directory string, root []byte) (map[string][]byte, error) {
	proofs := map[string][]byte{}
	state, err := trustedmetadata.New(root)
	if err != nil {
		return nil, err
	}
	for n := 0; n < 32; n++ {
		name := strconv.FormatInt(state.Root.Signed.Version+1, 10) + ".root.json"
		data, err := readBoundedRegular(filepath.Join(directory, "roots", name), 128<<10)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, err
		}
		if _, err = state.UpdateRoot(data); err != nil {
			return nil, err
		}
		proofs[name] = data
	}
	timestamp, err := readBoundedRegular(filepath.Join(directory, "metadata", "timestamp.json"), 64<<10)
	if err != nil {
		return nil, err
	}
	proofs["timestamp.json"] = timestamp
	tm, err := metadata.Timestamp().FromBytes(timestamp)
	if err != nil {
		return nil, err
	}
	snapshot, err := readBoundedRegular(filepath.Join(directory, "metadata", "snapshot.json"), 256<<10)
	if err != nil {
		return nil, err
	}
	if tm.Signed.Meta["snapshot.json"] == nil {
		return nil, ErrArtifactMismatch
	}
	snapshotName := "snapshot.json"
	if state.Root.Signed.ConsistentSnapshot {
		snapshotName = strconv.FormatInt(tm.Signed.Meta["snapshot.json"].Version, 10) + ".snapshot.json"
	}
	proofs[snapshotName] = snapshot
	sm, err := metadata.Snapshot().FromBytes(snapshot)
	if err != nil {
		return nil, err
	}
	targets, err := readBoundedRegular(filepath.Join(directory, "metadata", "targets.json"), 512<<10)
	if err != nil {
		return nil, err
	}
	if sm.Signed.Meta["targets.json"] == nil {
		return nil, ErrArtifactMismatch
	}
	targetsName := "targets.json"
	if state.Root.Signed.ConsistentSnapshot {
		targetsName = strconv.FormatInt(sm.Signed.Meta["targets.json"].Version, 10) + ".targets.json"
	}
	proofs[targetsName] = targets
	return proofs, nil
}

type bundleFetcher struct {
	prefix string
	files  map[string][]byte
}

func (f bundleFetcher) DownloadFile(raw string, limit int64, _ time.Duration) ([]byte, error) {
	if !strings.HasPrefix(raw, f.prefix) {
		return nil, ErrArtifactTarget
	}
	name := strings.TrimPrefix(raw, f.prefix)
	data, ok := f.files[name]
	if !ok {
		if strings.HasSuffix(name, ".root.json") {
			return nil, &metadata.ErrDownloadHTTP{StatusCode: http.StatusNotFound, URL: raw}
		}
		return nil, ErrArtifactMismatch
	}
	if int64(len(data)) > limit {
		return nil, ErrArtifactMismatch
	}
	return data, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ImportUpdateBundle authenticates exclusively from the supplied proof while
// retaining the receiving machine's trusted metadata floors. It never uses HTTP.
// Official installation callers must use protected receiver state and hold their
// existing update operation lock; a caller-owned cache is not installation authority.
func ImportUpdateBundle(ctx context.Context, repositoryURL, stateDirectory, bundlePath, destinationDirectory string, now time.Time) (VerifiedBundle, error) {
	return importUpdateBundle(ctx, repositoryURL, stateDirectory, bundlePath, destinationDirectory, now, trustedRoot, runtime.GOOS, runtime.GOARCH)
}
func importUpdateBundle(ctx context.Context, repositoryURL, stateDirectory, bundlePath, destinationDirectory string, now time.Time, root []byte, platform, architecture string) (result VerifiedBundle, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !filepath.IsAbs(bundlePath) || filepath.Clean(bundlePath) != bundlePath || !filepath.IsAbs(destinationDirectory) || filepath.Clean(destinationDirectory) != destinationDirectory {
		return result, ErrArtifactTarget
	}
	if err := secureDirectory(destinationDirectory); err != nil {
		return result, err
	}
	info, err := os.Lstat(bundlePath)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() > (512<<20)+maxBundleMetadata+(64<<10) {
		return result, ErrArtifactMismatch
	}
	f, err := os.Open(bundlePath)
	if err != nil {
		return result, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrArtifactMismatch
	}
	pending, err := os.CreateTemp(destinationDirectory, ".pb-bundle-")
	if err != nil {
		return result, err
	}
	defer pending.Close()
	defer func() {
		if resultErr != nil {
			os.Remove(pending.Name())
		}
	}()
	reader := tar.NewReader(&contextReader{ctx: ctx, r: f})
	proofs := map[string][]byte{}
	seen := map[string]bool{}
	total := int64(0)
	artifactSeen := false
	artifactHash := sha256.New()
	artifactLength := int64(0)
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, err
		}
		if h.Typeflag != tar.TypeReg || h.Size < 0 || seen[h.Name] || len(seen) >= 36 {
			return result, ErrArtifactMismatch
		}
		seen[h.Name] = true
		if h.Name == bundleArtifact {
			if h.Size < 1 || h.Size > 512<<20 {
				return result, ErrArtifactMismatch
			}
			artifactSeen = true
			artifactLength, err = io.Copy(io.MultiWriter(pending, artifactHash), reader)
			if err != nil {
				return result, err
			}
			if artifactLength != h.Size {
				return result, ErrArtifactMismatch
			}
		} else {
			if !strings.HasPrefix(h.Name, "metadata/") {
				return result, ErrArtifactMismatch
			}
			name := strings.TrimPrefix(h.Name, "metadata/")
			limit := metadataBundleLimit(name)
			if limit == 0 || h.Size > limit {
				return result, ErrArtifactMismatch
			}
			total += h.Size
			if total > maxBundleMetadata {
				return result, ErrArtifactMismatch
			}
			data, err := io.ReadAll(io.LimitReader(reader, limit+1))
			if err != nil {
				return result, err
			}
			proofs[name] = data
		}
	}
	if !artifactSeen {
		return result, ErrArtifactMismatch
	}
	source := bundleFetcher{prefix: strings.TrimRight(repositoryURL, "/") + "/metadata/", files: proofs}
	unlock, err := lockArtifactState(ctx, stateDirectory)
	if err != nil {
		return result, err
	}
	defer unlock()
	client, err := newArtifactVerifier(ctx, repositoryURL, stateDirectory, root, nil, source)
	if err != nil {
		return result, err
	}
	if err = refreshArtifactVerifier(client, stateDirectory); err != nil {
		return result, err
	}
	index, err := verifiedIndex(client, now, platform, architecture)
	if err != nil {
		return result, err
	}
	unlock()
	target, _ := index.Component("pb")
	if artifactLength != target.Length || hex.EncodeToString(artifactHash.Sum(nil)) != target.SHA256 {
		return result, ErrArtifactMismatch
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = pending.Sync(); err != nil {
		return result, err
	}
	if err = pending.Chmod(0700); err != nil {
		return result, err
	}
	if err = pending.Close(); err != nil {
		return result, err
	}
	return VerifiedBundle{Index: index, ArtifactPath: pending.Name()}, nil
}
func metadataBundleLimit(name string) int64 {
	if name == "timestamp.json" {
		return 64 << 10
	}
	for _, role := range []string{"root", "snapshot", "targets"} {
		suffix := "." + role + ".json"
		if name == role+".json" && role != "root" {
			if role == "snapshot" {
				return 256 << 10
			}
			return 512 << 10
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		version := strings.TrimSuffix(name, suffix)
		n, err := strconv.ParseInt(version, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != version {
			return 0
		}
		switch role {
		case "root":
			return 128 << 10
		case "snapshot":
			return 256 << 10
		case "targets":
			return 512 << 10
		}
	}
	return 0
}

// FetchCachedVerifiedReleaseIndex authenticates retained receiver metadata with
// the same TUF verifier and floors, without any network access. Expiry remains
// enforced; callers use this only for an explicitly offline native operation.
func FetchCachedVerifiedReleaseIndex(ctx context.Context, repositoryURL, stateDirectory string, now time.Time) (releaseindex.Index, error) {
	return fetchCachedVerifiedReleaseIndex(ctx, repositoryURL, stateDirectory, now, trustedRoot, runtime.GOOS, runtime.GOARCH)
}
func fetchCachedVerifiedReleaseIndex(ctx context.Context, repositoryURL, stateDirectory string, now time.Time, root []byte, platform, architecture string) (releaseindex.Index, error) {
	unlock, err := lockArtifactState(ctx, stateDirectory)
	if err != nil {
		return releaseindex.Index{}, err
	}
	defer unlock()
	proofs, err := bundleProofs(stateDirectory, root)
	if err != nil {
		return releaseindex.Index{}, err
	}
	source := bundleFetcher{prefix: strings.TrimRight(repositoryURL, "/") + "/metadata/", files: proofs}
	client, err := newArtifactVerifier(ctx, repositoryURL, stateDirectory, root, nil, source)
	if err != nil {
		return releaseindex.Index{}, err
	}
	if err := refreshArtifactVerifier(client, stateDirectory); err != nil {
		return releaseindex.Index{}, err
	}
	return verifiedIndex(client, now, platform, architecture)
}
