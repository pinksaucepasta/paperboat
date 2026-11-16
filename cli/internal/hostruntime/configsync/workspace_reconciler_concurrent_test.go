package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

// A file written while the first sync of an assignment is running must be published by the next pass, not recorded as
// already synced by the snapshot taken after the commit.
func TestFileWrittenDuringASyncIsPublishedByTheNextPass(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := resolvedTempDir(t)
	repositoryRoot := filepath.Join(root, "repository")
	homeRoot := filepath.Join(root, "home")
	configRoot := filepath.Join(root, "selected-config")
	stateRoot := filepath.Join(root, "state")
	for _, path := range []string{repositoryRoot, homeRoot, configRoot, stateRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(repositoryRoot, "keep.txt"), "base\n")
	repository, err := git.PlainInit(repositoryRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	base := commitAll(t, repository, "base")
	writeTestFile(t, filepath.Join(configRoot, "first.txt"), "first\n")
	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = []PathRule{{ID: "cfg", RepositoryPath: "cfg", LocalPath: configRoot, Kind: "directory"}}
	late := filepath.Join(configRoot, "late.txt")
	writeDuringSync := false
	reconciler, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{
		HomeRoot: homeRoot, StateRoot: stateRoot, Descriptor: descriptor,
		// The commit is the step between the plan's snapshot and the post-commit snapshot.
		Clock: func() time.Time {
			if !writeDuringSync {
				writeDuringSync = true
				writeTestFile(t, late, "late\n")
			}
			return time.Unix(2, 0).UTC()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prepared, err := reconciler.Reconcile(ctx, repositoryRoot, RemoteSnapshot{Revision: base})
	if err != nil || !prepared.HasChanges || !writeDuringSync {
		t.Fatalf("first sync = %#v, wrote during sync %v, %v", prepared, writeDuringSync, err)
	}
	if err := reconciler.PublicationCommitted(ctx, prepared, prepared.CommitID); err != nil {
		t.Fatal(err)
	}
	if state, known := reconciler.ObservedLocalFile("cfg/late.txt"); known && state.Hash != "" {
		t.Fatalf("late file is recorded as reconciled: %#v", state)
	}
	next, err := reconciler.Reconcile(ctx, repositoryRoot, RemoteSnapshot{Revision: prepared.CommitID})
	if err != nil || !next.HasChanges {
		t.Fatalf("next pass = %#v, %v; the late file was not published", next, err)
	}
	if _, err := os.Stat(filepath.Join(repositoryRoot, "cfg", "late.txt")); err != nil {
		t.Fatal(err)
	}
}
