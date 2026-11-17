// Package releases reads the release information the server publishes to clients: the Current view
// derived from the signed release targets (version and per-platform assets), a readiness check of the
// release directory, and the shared rules for which release versions and platform and architecture
// pairs are valid.
package releases

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/releaseindex"
	"github.com/pinksaucepasta/paperboat/shared/releasepolicy"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CurrentSchemaV1 describes the in-memory view derived from TUF targets.
// The release origin no longer publishes a separate current.json selector.
const CurrentSchemaV1 = "paperboat.release-current/v1"

const tufAssetSchemaV1 = "paperboat.tuf-asset/v1"

var (
	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

var supportedPlatformArchitectures = [...]struct {
	platform     string
	architecture string
}{
	{platform: "darwin", architecture: "arm64"},
	{platform: "linux", architecture: "amd64"},
	{platform: "linux", architecture: "arm64"},
	{platform: "windows", architecture: "amd64"},
	{platform: "windows", architecture: "arm64"},
}

type Current struct {
	Schema     string           `json:"schema"`
	Version    string           `json:"version"`
	Repository string           `json:"repository"`
	Assets     map[string]Asset `json:"assets"`
}

type Asset struct {
	Version      string `json:"version"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	Format       string `json:"format"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	Length       int64  `json:"length"`
}

type releaseReadFailure struct {
	message string
	cause   error
}

func (e releaseReadFailure) Error() string { return e.message }
func (e releaseReadFailure) Unwrap() error { return e.cause }

func releaseReadError(message string, cause error) error {
	if cause == nil {
		return errors.New(message)
	}
	return releaseReadFailure{message: message, cause: cause}
}

func ReadCurrent(directory string) (result Current, resultErr error) {
	path := filepath.Join(directory, "tuf", "metadata", "targets.json")
	info, err := os.Lstat(path)
	if err != nil {
		return Current{}, releaseReadError("release targets metadata is unavailable", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > 512<<10 {
		return Current{}, errors.New("release targets metadata is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return Current{}, releaseReadError("release targets metadata is unavailable", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			result = Current{}
			cause := closeErr
			message := "release targets metadata is unavailable"
			if resultErr != nil {
				cause = errors.Join(resultErr, closeErr)
				message = "release targets metadata is invalid"
			}
			resultErr = releaseReadFailure{message: message, cause: cause}
		}
	}()
	var metadata struct {
		Signed struct {
			Targets map[string]tufAssetTarget `json:"targets"`
		} `json:"signed"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 512<<10+1))
	var extra any
	if err := decoder.Decode(&metadata); err != nil {
		return Current{}, releaseReadError("release targets metadata is invalid", err)
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Current{}, releaseReadError("release targets metadata is invalid", err)
		}
		return Current{}, errors.New("release targets metadata is invalid")
	}
	if len(metadata.Signed.Targets) != len(supportedPlatformArchitectures) {
		return Current{}, errors.New("release targets metadata is invalid")
	}
	current := Current{Schema: CurrentSchemaV1, Assets: make(map[string]Asset, len(metadata.Signed.Targets))}
	for name, target := range metadata.Signed.Targets {
		var custom struct {
			Version      string `json:"version"`
			Repository   string `json:"repository"`
			Platform     string `json:"platform"`
			Architecture string `json:"architecture"`
			Format       string `json:"format"`
			URL          string `json:"url"`
			SHA256       string `json:"sha256"`
			Length       int64  `json:"length"`
		}
		if json.Unmarshal(target.Custom, &custom) != nil {
			return Current{}, errors.New("release target custom metadata is invalid")
		}
		if !validVersion(custom.Version) {
			return Current{}, errors.New("release target version is invalid")
		}
		if current.Repository == "" {
			current.Repository = custom.Repository
		}
		if current.Repository != custom.Repository {
			return Current{}, errors.New("release targets have inconsistent identity")
		}
		// The origin advances qualified platform targets independently. Version
		// is the newest published release; enrollment selects the asset's version.
		if newerReleaseVersion(custom.Version, current.Version) {
			current.Version = custom.Version
		}
		asset := Asset{Version: custom.Version, Platform: custom.Platform, Architecture: custom.Architecture, Format: custom.Format, URL: custom.URL, SHA256: custom.SHA256, Length: custom.Length}
		if target.Length != asset.Length || target.Hashes["sha256"] != asset.SHA256 || !validAssetTargetCustom(target.Custom, asset.Version, current.Repository, name, asset) {
			return Current{}, errors.New("release target metadata is invalid")
		}
		current.Assets[name] = asset
	}
	if current.Validate() != nil {
		return Current{}, errors.New("release targets metadata is invalid")
	}
	return current, nil
}

func Ready(directory string) error {
	current, err := ReadCurrent(directory)
	if err != nil {
		return err
	}
	for _, relative := range []string{"install", "windows", "tuf/metadata/root.json", "tuf/metadata/timestamp.json", "tuf/metadata/snapshot.json", "tuf/metadata/targets.json"} {
		info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(relative)))
		if err != nil {
			return releaseReadError("release bundle is incomplete", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 {
			return errors.New("release bundle is incomplete")
		}
	}
	// The origin is metadata-only. A target blob here would create a second
	// binary distribution path and is rejected even if it is otherwise valid.
	if info, err := os.Lstat(filepath.Join(directory, "tuf", "targets")); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("release origin contains a non-directory TUF targets path")
		}
		entries, readErr := os.ReadDir(filepath.Join(directory, "tuf", "targets"))
		if readErr != nil {
			return releaseReadError("release origin must not contain TUF target blobs", readErr)
		}
		if len(entries) != 0 {
			return errors.New("release origin must not contain TUF target blobs")
		}
	} else if !os.IsNotExist(err) {
		return releaseReadError("release origin TUF targets path is unavailable", err)
	}
	body, err := os.ReadFile(filepath.Join(directory, "tuf", "metadata", "targets.json"))
	if err != nil {
		return releaseReadError("release targets metadata is unavailable", err)
	}
	if len(body) > 512<<10 {
		return errors.New("release targets metadata is unavailable")
	}
	var metadata struct {
		Signed struct {
			Targets map[string]tufAssetTarget `json:"targets"`
		} `json:"signed"`
	}
	if json.Unmarshal(body, &metadata) != nil {
		return errors.New("release targets metadata is invalid")
	}
	if len(metadata.Signed.Targets) != len(current.Assets) {
		return errors.New("release targets metadata has an unexpected asset set")
	}
	for name, asset := range current.Assets {
		target, ok := metadata.Signed.Targets[name]
		if !ok || target.Length != asset.Length || target.Hashes["sha256"] != asset.SHA256 || !validAssetTargetCustom(target.Custom, asset.Version, current.Repository, name, asset) {
			return errors.New("release target metadata is incomplete")
		}
	}
	for name := range metadata.Signed.Targets {
		if _, ok := current.Assets[name]; !ok {
			return errors.New("release targets metadata contains an unsupported target")
		}
	}
	return nil
}

type tufAssetTarget struct {
	Length int64             `json:"length"`
	Hashes map[string]string `json:"hashes"`
	Custom json.RawMessage   `json:"custom"`
}

// publishedReleaseIndex is the release-index document the publisher signs into each TUF asset; the cli publisher and every
// runtime validate it with the one validator in shared/releaseindex, and so does the origin before it serves a bundle.
type publishedReleaseIndex = releaseindex.Index

type publishedReleaseTarget = releaseindex.Target

func (c Current) Validate() error {
	if c.Schema != CurrentSchemaV1 || !validVersion(c.Version) || !validCurrentRepository(c.Repository) || len(c.Assets) != len(supportedPlatformArchitectures) {
		return errors.New("invalid current release manifest")
	}
	for _, targetPlatform := range supportedPlatformArchitectures {
		name := releaseindex.AssetName(targetPlatform.platform, targetPlatform.architecture)
		asset, ok := c.Assets[name]
		if !ok || !validVersion(asset.Version) || asset.Platform != targetPlatform.platform || asset.Architecture != targetPlatform.architecture || asset.Format != assetFormat(targetPlatform.platform) || asset.Length < 1 || asset.Length > 512<<20 || !releasepolicy.IsDigest(asset.SHA256) || !releaseindex.ValidDownloadURL(asset.URL, c.Repository, asset.Version, name) {
			return errors.New("invalid current release asset")
		}
	}
	return nil
}

func newerReleaseVersion(candidate, previous string) bool {
	if previous == "" {
		return true
	}
	left, right := strings.Split(candidate, "."), strings.Split(previous, ".")
	for index := range left {
		if len(left[index]) != len(right[index]) {
			return len(left[index]) > len(right[index])
		}
		if left[index] != right[index] {
			return left[index] > right[index]
		}
	}
	return false
}

func validAssetTargetCustom(raw json.RawMessage, version, repository, name string, asset Asset) bool {
	var custom struct {
		Schema       string          `json:"schema"`
		Kind         string          `json:"kind"`
		Version      string          `json:"version"`
		Platform     string          `json:"platform"`
		Architecture string          `json:"architecture"`
		Format       string          `json:"format"`
		AssetName    string          `json:"asset_name"`
		Repository   string          `json:"repository"`
		URL          string          `json:"url"`
		SHA256       string          `json:"sha256"`
		Length       int64           `json:"length"`
		ReleaseIndex json.RawMessage `json:"release_index"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&custom) != nil || decoder.Decode(&extra) != io.EOF || custom.Schema != tufAssetSchemaV1 || custom.Kind != "github-release-asset" || custom.Version != version || custom.Platform != asset.Platform || custom.Architecture != asset.Architecture || custom.Format != asset.Format || custom.AssetName != name || custom.Repository != repository || custom.URL != asset.URL || custom.SHA256 != asset.SHA256 || custom.Length != asset.Length {
		return false
	}
	index, ok := decodePublishedReleaseIndex(custom.ReleaseIndex, time.Now().UTC())
	if !ok || index.Version != version || index.Platform != asset.Platform || index.Architecture != asset.Architecture || index.BinaryFormat != asset.Format || len(index.Targets) != 1 {
		return false
	}
	target := index.Targets[0]
	return target.Component == "pb" && target.TargetPath == name && target.AssetName == name && target.Repository == repository && target.DownloadURL == asset.URL && target.SHA256 == asset.SHA256 && target.Length == asset.Length
}

func decodePublishedReleaseIndex(raw json.RawMessage, now time.Time) (publishedReleaseIndex, bool) {
	index, err := releaseindex.Decode(bytes.NewReader(raw), now)
	if err != nil || !SupportedPlatformArchitecture(index.Platform, index.Architecture) {
		return publishedReleaseIndex{}, false
	}
	return index, true
}

func validCurrentRepository(value string) bool {
	return githubRepositoryPattern.MatchString(value)
}

func assetFormat(platform string) string {
	switch platform {
	case "darwin":
		return "macho"
	case "linux":
		return "elf"
	case "windows":
		return "pe"
	default:
		return ""
	}
}

// SupportedPlatformArchitecture reports whether the public release pipeline
// publishes native bytes for the exact platform and architecture pair.
func SupportedPlatformArchitecture(platform, architecture string) bool {
	for _, target := range supportedPlatformArchitectures {
		if platform == target.platform && architecture == target.architecture {
			return true
		}
	}
	return false
}

func validVersion(version string) bool {
	if version == "" || len(version) > 64 || strings.ContainsAny(version, "/\\\x00\r\n") {
		return false
	}
	for _, character := range version {
		if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && !strings.ContainsRune("._-", character) {
			return false
		}
	}
	return true
}
