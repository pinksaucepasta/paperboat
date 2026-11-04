package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	sessionStateSQL = regexp.MustCompile("pg_(try_)?advisory_(lock|unlock)(_shared)?\\b|[\"`]LISTEN\\b")
	// poolFor is how a file reaches the direct pool; leader.go takes it as the DB field, which the server wires
	// from store.Direct() (checked below).
	directPool  = regexp.MustCompile(`\.Direct\(\)|\bdirect\.Acquire\(|\bl\.DB\.Acquire\(`)
	lineComment = regexp.MustCompile(`(?m)^\s*//.*$`)
	pooledUse   = regexp.MustCompile(`\.Pool\(\)\.(Acquire|Begin)|\bpool\.Acquire\(|\.SQL\(\)\.Conn\(`)
)

// F21-15: behind a transaction-pooling PgBouncer a session advisory lock or LISTEN on the pooled pool leaks the
// lock or never wakes. Every server source file that runs one must take its connection from the direct pool and
// never from the pooled one, and the one place that hands the leader its pool must pass Direct().
func TestSessionAdvisoryLocksAndListenUseTheDirectPool(t *testing.T) {
	root := filepath.Join("..", "..")
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := lineComment.ReplaceAllString(string(body), "")
		if !sessionStateSQL.MatchString(text) || strings.Contains(path, "dbsqlc") {
			return nil
		}
		found++
		if !directPool.MatchString(text) {
			t.Errorf("%s runs a session advisory lock or LISTEN without acquiring from the direct pool", path)
		}
		if pooledUse.MatchString(text) {
			t.Errorf("%s runs a session advisory lock or LISTEN and also acquires from the pooled pool; keep session state in its own file", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 6 {
		t.Fatalf("found %d session-state files, want at least the 6 known ones: the scan is broken", found)
	}
	lifecycle, err := os.ReadFile(filepath.Join(root, "internal", "app", "lifecycle.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`PostgresLeader\{\s*DB:\s*store\.Direct\(\)`).Match(lifecycle) {
		t.Error("the worker leader must be built on store.Direct()")
	}
}
