package main

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

func testOfflineRoot(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	rootKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	t.Setenv("PAPERBOAT_TUF_ONLINE_KEY", base64.RawStdEncoding.EncodeToString(online.Seed()))
	root := metadata.Root(time.Now().UTC().Add(730 * 24 * time.Hour))
	root.Signed.ConsistentSnapshot = true
	for _, role := range []string{"root", "targets", "snapshot", "timestamp"} {
		private := online
		if role == "root" {
			private = rootKey
		}
		key, err := metadata.KeyFromPublicKey(private.Public())
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Signed.AddKey(key, role); err != nil {
			t.Fatal(err)
		}
	}
	signTestRoot(t, root, rootKey)
	path := filepath.Join(t.TempDir(), "1.root.json")
	if err := os.WriteFile(path, mustMetadataBytes(root), 0600); err != nil {
		t.Fatal(err)
	}
	return path, rootKey
}

func signTestRoot(t *testing.T, root *metadata.Metadata[metadata.RootType], private ed25519.PrivateKey) {
	t.Helper()
	signer, err := signature.LoadSigner(private, crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Sign(signer); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeUsesOfflineSignedRootAndOneOnlineKey(t *testing.T) {
	t.Setenv("PAPERBOAT_TUF_CI", "1")
	rootPath, _ := testOfflineRoot(t)
	repository := filepath.Join(t.TempDir(), "repository")
	if err := run([]string{"init", "-repository", repository, "-trusted-root", rootPath}); err != nil {
		t.Fatal(err)
	}
	root, targets, snapshot, timestamp, err := loadSet(repository)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustMetadataBytes(root), readTestMetadata(t, rootPath)) {
		t.Fatal("initialization changed the offline signed root")
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, ".signing-state.json")); !os.IsNotExist(err) {
		t.Fatal("online publisher persisted signing state")
	}
	t.Setenv("PAPERBOAT_TUF_ONLINE_KEY", base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{99}, ed25519.SeedSize)))
	refused := filepath.Join(t.TempDir(), "refused")
	if err := run([]string{"init", "-repository", refused, "-trusted-root", rootPath}); err == nil {
		t.Fatal("unauthorized online key initialized repository")
	}
	if _, err := os.Stat(refused); !os.IsNotExist(err) {
		t.Fatal("unauthorized initialization mutated repository")
	}
}

func initializeTestRepository(t *testing.T, repository string) error {
	t.Helper()
	rootPath, _ := testOfflineRoot(t)
	return initialize(repository, rootPath)
}
