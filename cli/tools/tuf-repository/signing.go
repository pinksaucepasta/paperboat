package main

import (
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

func initialize(repo, rootPath string) error {
	repo, err := validateRepository(repo, false)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(repo); err == nil {
		return errors.New("repository already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return errors.New("trusted root path must be absolute and clean")
	}
	root, err := metadata.Root().FromFile(rootPath)
	if err != nil {
		return fmt.Errorf("load offline signed root: %w", err)
	}
	if root.Signed.Version != 1 || !root.Signed.ConsistentSnapshot {
		return errors.New("initial root must be version1 with consistent snapshots")
	}
	if err := validateOnlineSigner(root); err != nil {
		return err
	}
	targets := metadata.Targets(time.Now().UTC().Add(90 * 24 * time.Hour))
	if err := sign(targets); err != nil {
		return err
	}
	snapshot := metadata.Snapshot(time.Now().UTC().Add(7 * 24 * time.Hour))
	snapshot.Signed.Meta["targets.json"] = metadata.MetaFile(targets.Signed.Version)
	if err := sign(snapshot); err != nil {
		return err
	}
	timestamp := metadata.Timestamp(time.Now().UTC().Add(24 * time.Hour))
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(snapshot.Signed.Version)
	if err := sign(timestamp); err != nil {
		return err
	}
	return writeSet(repo, root, targets, snapshot, timestamp)
}

func sign[T metadata.Roles](value *metadata.Metadata[T]) error {
	private, err := loadOnlineKey()
	if err != nil {
		return err
	}
	signer, err := signature.LoadSigner(private, crypto.Hash(0))
	if err != nil {
		return err
	}
	_, err = value.Sign(signer)
	return err
}

func loadOnlineKey() (ed25519.PrivateKey, error) {
	seed, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("PAPERBOAT_TUF_ONLINE_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("PAPERBOAT_TUF_ONLINE_KEY must contain an unpadded base64 Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// The root is the only authority for online signing. No local role mapping or
// offline private key is needed by a publication or refresh job.
func validateOnlineSigner(root *metadata.Metadata[metadata.RootType]) error {
	if err := verifyRoot(root); err != nil {
		return err
	}
	if !root.Signed.Expires.After(time.Now().UTC()) {
		return errors.New("trusted root has expired")
	}
	private, err := loadOnlineKey()
	if err != nil {
		return err
	}
	key, err := metadata.KeyFromPublicKey(private.Public())
	if err != nil {
		return err
	}
	id, err := key.ID()
	if err != nil {
		return err
	}
	offline := root.Signed.Roles["root"]
	if offline == nil || offline.Threshold != 1 || len(offline.KeyIDs) != 1 || offline.KeyIDs[0] == id {
		return errors.New("offline root must authorize one distinct root key")
	}
	for _, role := range []string{"targets", "snapshot", "timestamp"} {
		configured := root.Signed.Roles[role]
		if configured == nil || configured.Threshold != 1 || len(configured.KeyIDs) != 1 || configured.KeyIDs[0] != id {
			return fmt.Errorf("online key is not the sole authorized signer for %s", role)
		}
	}
	return nil
}
