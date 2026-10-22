package installsource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !verifiedFiles.matches(path, identity, source) {
		t.Fatal("a verified executable was not remembered")
	}
	other := source
	other.SHA256 = strings.Repeat("0", 64)
	if verifiedFiles.matches(path, identity, other) {
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
