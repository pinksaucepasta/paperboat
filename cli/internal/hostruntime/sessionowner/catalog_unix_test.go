//go:build linux || darwin

package sessionowner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogCapabilitySurvivesArchivedReopenAndRejectsRedirectedStore(t *testing.T) {
	d := Descriptor{RuntimeRoot: t.TempDir(), Generation: strings.Repeat("a", 32), UID: os.Getuid(), GID: os.Getgid()}
	if err := os.Chmod(d.RuntimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareCatalogCapability(d); err != nil {
		t.Fatal(err)
	}
	first, err := ReadCapability(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCatalogCapability(d); err != nil {
		t.Fatal(err)
	}
	second, err := ReadCapability(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("archive reopening replaced private owner identity")
	}
	redirect := Descriptor{RuntimeRoot: t.TempDir(), Generation: strings.Repeat("b", 32), UID: os.Getuid(), GID: os.Getgid()}
	if err := os.Chmod(redirect.RuntimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(redirect.RuntimeRoot, "session-owners")); err != nil {
		t.Fatal(err)
	}
	if err := prepareCatalogCapability(redirect); err == nil {
		t.Fatal("redirected private store accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, redirect.Generation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redirect target mutated: %v", err)
	}
}
func TestCatalogRejectsAccountWritablePublicationRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	c := Catalog{Root: root, ConfigRoot: root, User: "test", Group: "test", Maximum: 2}
	if _, err := c.ReadAll(context.Background(), "u1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("untrusted catalog root accepted: %v", err)
	}
}
