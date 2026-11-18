package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/releaseindex"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// selfhostTargetPrefix names the self-host bundle's targets: selfhost/<version>/paperboat-selfhost-linux-<arch>.tar.gz.
// pbh verifies an update against them (helper/internal/selfupdate); they carry no pb release policy.
const selfhostTargetPrefix = "selfhost/"

var selfhostTargetPattern = regexp.MustCompile(`^selfhost/([0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}\.[0-9]{1,6})/(paperboat-selfhost-linux-(?:amd64|arm64)\.tar\.gz)$`)

// selfhostTargetCustom says where the bundle's bytes are: the selfhost-<version> GitHub release that carries the
// signed SHA256SUMS. pbh downloads from this URL and checks the bytes against the target's length and sha256.
type selfhostTargetCustom struct {
	Repository string `json:"repository"`
	URL        string `json:"url"`
}

func selfhostDownloadURL(repository, version, file string) string {
	return "https://github.com/" + repository + "/releases/download/selfhost-" + version + "/" + file
}

func validateSelfhostTarget(name string, info *metadata.TargetFiles) error {
	parts := selfhostTargetPattern.FindStringSubmatch(name)
	if parts == nil || info == nil || info.Length < 1 || info.Length > 128<<20 || len(info.Hashes["sha256"]) != sha256.Size || info.Custom == nil {
		return fmt.Errorf("self-host TUF target %s is invalid", name)
	}
	var custom selfhostTargetCustom
	if json.Unmarshal(*info.Custom, &custom) != nil || !releaseindex.ValidDownloadURL(custom.URL, custom.Repository, "selfhost-"+parts[1], parts[2]) {
		return fmt.Errorf("self-host TUF target %s has no valid release download URL", name)
	}
	return nil
}

// addSelfhost signs the self-host bundle of version into the targets set, replacing the previous bundle's targets.
func addSelfhost(repo, version, artifacts string) error {
	root, targets, snapshot, timestamp, err := loadSet(repo)
	if err != nil {
		return err
	}
	if err := verifyRoot(root); err != nil {
		return err
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return fmt.Errorf("verify existing targets: %w", err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return err
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return err
	}
	if err := validateTargetsForPublication(targets, time.Now().UTC()); err != nil {
		return err
	}
	if err := validateOnlineSigner(root); err != nil {
		return err
	}
	for name := range targets.Signed.Targets {
		if strings.HasPrefix(name, selfhostTargetPrefix) {
			delete(targets.Signed.Targets, name)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		file := "paperboat-selfhost-linux-" + arch + ".tar.gz"
		name := selfhostTargetPrefix + version + "/" + file
		info, err := metadata.TargetFile().FromFile(filepath.Join(artifacts, file), "sha256")
		if err != nil {
			return fmt.Errorf("self-host bundle %s: %w", file, err)
		}
		custom, err := json.Marshal(selfhostTargetCustom{Repository: releaseRepository(), URL: selfhostDownloadURL(releaseRepository(), version, file)})
		if err != nil {
			return err
		}
		raw := json.RawMessage(custom)
		info.Path, info.Custom = name, &raw
		if err := validateSelfhostTarget(name, info); err != nil {
			return err
		}
		targets.Signed.Targets[name] = info
	}
	if len(targets.Signed.Targets) == 0 {
		return errors.New("no targets to sign")
	}
	return signTargetsSet(repo, root, targets, snapshot, timestamp)
}
