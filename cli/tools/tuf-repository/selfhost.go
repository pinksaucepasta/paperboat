package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// selfhostTargetPrefix names the self-host bundle's targets: selfhost/<version>/paperboat-selfhost-linux-<arch>.tar.gz.
// pbh verifies an update against them (helper/internal/selfupdate); they carry no pb release policy.
const selfhostTargetPrefix = "selfhost/"

var selfhostTargetPattern = regexp.MustCompile(`^selfhost/[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}\.[0-9]{1,6}/paperboat-selfhost-linux-(amd64|arm64)\.tar\.gz$`)

func validateSelfhostTarget(name string, info *metadata.TargetFiles) error {
	if !selfhostTargetPattern.MatchString(name) || info == nil || info.Length < 1 || info.Length > 128<<20 || len(info.Hashes["sha256"]) != sha256.Size {
		return fmt.Errorf("self-host TUF target %s is invalid", name)
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
		info.Path = name
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
