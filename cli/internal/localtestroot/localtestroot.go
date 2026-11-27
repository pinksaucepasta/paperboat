// Package localtestroot is the test-binary-only seam that keeps Windows unit
// tests away from an installed Paperboat's state and pipes.
package localtestroot

import (
	"os"
	"path/filepath"
	"testing"
)

// EnvName names an absolute, clean directory that replaces the per-user
// LocalAppData root and salts the per-user pipe names. It is honored only
// inside a Go test binary: a shipped pb never trusts it.
const EnvName = "PAPERBOAT_TEST_LOCAL_ROOT"

// Root returns the test local root, or "" outside a test binary or when unset or invalid.
func Root() string {
	if !testing.Testing() {
		return ""
	}
	root := os.Getenv(EnvName)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return ""
	}
	return root
}

// Isolate creates a private directory, points EnvName at it and returns a
// cleanup. For TestMain: re-executed helper processes inherit the variable.
func Isolate() (func(), error) {
	dir, err := os.MkdirTemp("", "pb-local-root-")
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	if err := os.Setenv(EnvName, dir); err != nil {
		return nil, err
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}
