package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

// The pull half sees the other machine's edit before that machine published the merge: the conflict is frozen at the
// base the disagreement started from. When the remote then moves to the merged file, the freeze is over (it is another
// disagreement) and the merge, made against the frozen ancestor, equals the remote, so the remote is applied.
func TestPullHalfLeavesAStaleFreezeWhenTheRemoteMovesToTheMerge(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := resolvedTempDir(t)
	repositoryRoot := filepath.Join(root, "repository")
	homeRoot := filepath.Join(root, "home")
	configRoot := filepath.Join(root, "selected-config")
	stateRoot := filepath.Join(root, "state", "pull")
	for _, path := range []string{repositoryRoot, homeRoot, stateRoot, configRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	local := filepath.Join(configRoot, "config.txt")
	for _, path := range []string{filepath.Join(repositoryRoot, "config.txt"), local} {
		writeTestFile(t, path, "one\r\ntwo\r\nthree\r\n")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := git.PlainInit(repositoryRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := commitAll(t, repository, "base")
	descriptor := testRuntimeDescriptor()
	descriptor.Mode = ModePullOnly
	descriptor.PathRules = []PathRule{{ID: "config", RepositoryPath: "config.txt", LocalPath: local, Kind: "file"}}
	reconciler, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{HomeRoot: homeRoot, StateRoot: stateRoot, Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	round := func(revision string) PreparedPublication {
		t.Helper()
		prepared, err := reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: revision})
		if err != nil {
			t.Fatal(err)
		}
		if err := reconciler.PublicationCommitted(context.Background(), prepared, revision); err != nil {
			t.Fatal(err)
		}
		return prepared
	}
	round(baseRevision)

	writeTestFile(t, local, "ONE\r\ntwo\r\nthree\r\n")
	if err := os.Chmod(local, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repositoryRoot, "config.txt"), "one\r\ntwo\r\nTHREE\r\n")
	round(commitAll(t, repository, "other machine edit"))
	if conflicts := reconciler.Diagnostics().Conflicts; len(conflicts) != 1 || conflicts[0].Reason != "concurrent_update" {
		t.Fatalf("conflicts after the first round = %#v", conflicts)
	}

	writeTestFile(t, filepath.Join(repositoryRoot, "config.txt"), "ONE\r\ntwo\r\nTHREE\r\n")
	round(commitAll(t, repository, "other machine merged"))
	value, err := os.ReadFile(local)
	if err != nil || string(value) != "ONE\r\ntwo\r\nTHREE\r\n" {
		t.Fatalf("target = %q, %v", value, err)
	}
	if conflicts := reconciler.Diagnostics().Conflicts; len(conflicts) != 0 {
		t.Fatalf("conflicts after the merge arrived = %#v", conflicts)
	}
}
