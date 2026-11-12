//go:build unix

package configsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

// A Windows working tree stages an executable file as a plain one; keepExecutableIntent undoes that.
func TestKeepExecutableIntentStagesAnEditedExecutableFileAsExecutable(t *testing.T) {
	root := resolvedTempDir(t)
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho v1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "initial")
	// What a Windows checkout and edit leave behind: new content, no executable bit.
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o644); err != nil {
		t.Fatal(err)
	}
	worktree, _ := repo.Worktree()
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if err := keepExecutableIntent(repo); err != nil {
		t.Fatal(err)
	}
	index, err := repo.Storer.Index()
	if err != nil || len(index.Entries) != 1 || index.Entries[0].Mode.String() != "0100755" {
		t.Fatalf("staged mode %v: %v", index.Entries, err)
	}
}
