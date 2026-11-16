package installsource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuppliedSourceBindsBytesAndUpdatePolicy(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, distribution := range []string{Custom, Official} {
		source, err := Inspect(path, "development", distribution)
		if err != nil {
			t.Fatal(err)
		}
		if source.AutomaticUpdates != (distribution == Official) {
			t.Fatal("incorrect update policy")
		}
		if err := source.Verify(path); err != nil {
			t.Fatal(err)
		}
		if distribution == Custom {
			source.AutomaticUpdates = true
			if source.Validate() == nil {
				t.Fatal("custom source enabled automatic replacements")
			}
		}
	}
	source, err := Inspect(path, "development", Custom)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	if source.Verify(path) == nil {
		t.Fatal("same-length changed binary accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := source.Verify(missing); err == nil || !errors.Is(err, ErrInvalid) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing binary result=%v", err)
	} else if strings.Contains(err.Error(), filepath.Dir(missing)) {
		t.Fatalf("local path escaped in verification error: %v", err)
	}
}

// A verified executable is not re-hashed while its on-disk identity is unchanged, and any
// rewrite is caught even when size and mtime are restored: only its ctime then differs.
func TestVerifyCachesByFileIdentityAndRehashesAfterAnyRewrite(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := Inspect(path, "development", Custom)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Verify(path); err != nil {
		t.Fatal(err)
	}
	identity, cacheable := fileIdentityOf(path)
	if !cacheable {
		t.Skip("no inode/ctime identity on this platform; every Verify hashes")
	}
	if entry := rememberedEntry(path); entry.identity != identity || entry.length != source.Length || entry.sha256 != source.SHA256 {
		t.Fatal("a verified executable was not remembered")
	}
	other := source
	other.SHA256 = strings.Repeat("0", 64)
	if other.Verify(path) == nil {
		t.Fatal("a remembered file matched a different source")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if source.Verify(path) == nil {
		t.Fatal("a rewritten executable with restored size and mtime was accepted")
	}
}

// VerifyDigest is what the Windows service definitions use: a match is remembered for the file's identity (two
// callers with different Source fields share it), a wrong length or digest is refused, a rewrite is hashed again.
func TestVerifyDigestRemembersAMatchAndRefusesAnythingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	body := []byte("executable bytes")
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if err := VerifyDigest(path, int64(len(body)), digest); err != nil {
		t.Fatal(err)
	}
	if identity, cacheable := fileIdentityOf(path); cacheable {
		if entry := rememberedEntry(path); entry.identity != identity || entry.length != int64(len(body)) || entry.sha256 != digest {
			t.Fatal("a verified file was not remembered")
		}
	}
	if err := VerifyDigest(path, int64(len(body))+1, digest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong length: %v", err)
	}
	if err := VerifyDigest(path, int64(len(body)), strings.Repeat("0", 64)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong digest: %v", err)
	}
}

// Digest follows the file: the remembered value is used while its identity is unchanged and never after a rewrite.
func TestDigestIsRehashedAfterARewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	for _, body := range []string{"first body", "other body"} {
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		for range 2 {
			length, got, err := Digest(path)
			if err != nil || length != int64(len(body)) || got != hex.EncodeToString(sum[:]) {
				t.Fatalf("Digest(%q) = %d %s %v", body, length, got, err)
			}
		}
	}
}

func rememberedEntry(path string) verifiedFile {
	verifiedFiles.mu.Lock()
	defer verifiedFiles.mu.Unlock()
	return verifiedFiles.entries[path]
}

// A file changed within racyWindow of its hash is hashed again: a same-size rewrite in the same file-time tick keeps
// the identity (Windows run 43960352441: TestDigestIsRehashedAfterARewrite got the first body's digest).
func TestDigestOfARecentlyChangedFileIsNotServedFromTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("body"), 0700); err != nil {
		t.Fatal(err)
	}
	identity, cacheable := fileIdentityOf(path)
	if !cacheable {
		t.Skip("no file identity on this platform; every Digest hashes")
	}
	cache := &verificationCache{entries: map[string]verifiedFile{}}
	cache.remember(path, identity, identity.changed(), 4, "racy")
	if _, _, ok := cache.lookup(path, identity); ok {
		t.Fatal("an entry hashed in the same tick as the file's last change was served")
	}
	cache.remember(path, identity, identity.changed().Add(racyWindow+time.Millisecond), 4, "settled")
	if _, sum, ok := cache.lookup(path, identity); !ok || sum != "settled" {
		t.Fatal("an entry hashed well after the file's last change was not served")
	}
}
