package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

// A file the other machine created arrives through the pull half. Edited here before the push half's next pass, the push
// half has no ancestor for it, so the other machine's edit and this one conflict forever (X-03 M>L: L kept B5, never merged).
// The pull half recorded the file when it wrote it, so that is the ancestor, as git's last fetched state is.
func TestPushHalfMergesAFileThePullHalfBroughtIn(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := resolvedTempDir(t)
	repositoryRoot := filepath.Join(root, "repository")
	homeRoot := filepath.Join(root, "home")
	configRoot := filepath.Join(root, "selected-config")
	assignmentRoot := filepath.Join(root, "state")
	for _, path := range []string{repositoryRoot, homeRoot, configRoot, filepath.Join(assignmentRoot, "pull"), filepath.Join(assignmentRoot, "push")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(configRoot, "config.txt")
	setFile := func(path, content string) {
		writeTestFile(t, path, content)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	newReconciler := func(name string, mode AssignmentMode, config WorkspaceReconcilerConfig) *PlaintextWorkspaceReconciler {
		descriptor := testRuntimeDescriptor()
		descriptor.Mode = mode
		descriptor.PathRules = []PathRule{{ID: "config", RepositoryPath: "config.txt", LocalPath: target, Kind: "file"}}
		config.HomeRoot, config.StateRoot, config.Descriptor = homeRoot, filepath.Join(assignmentRoot, name), descriptor
		reconciler, err := newTestFileReconciler(t, config)
		if err != nil {
			t.Fatal(err)
		}
		return reconciler
	}
	sync := func(reconciler *PlaintextWorkspaceReconciler, revision string) PreparedPublication {
		prepared, err := reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: revision})
		if err != nil {
			t.Fatal(err)
		}
		return prepared
	}
	commit := func(reconciler *PlaintextWorkspaceReconciler, prepared PreparedPublication, revision string) {
		if err := reconciler.PublicationCommitted(context.Background(), prepared, revision); err != nil {
			t.Fatal(err)
		}
	}
	setFile(filepath.Join(repositoryRoot, "README"), "not synced\n")
	repository, err := git.PlainInit(repositoryRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	empty := commitAll(t, repository, "before the file")
	push := newReconciler("push", ModePushOnly, WorkspaceReconcilerConfig{MergeConcurrentEdits: true})
	commit(push, sync(push, empty), empty)

	setFile(filepath.Join(repositoryRoot, "config.txt"), "l1\r\nl2\r\nl3\r\nl4\r\nl5\r\n")
	created := commitAll(t, repository, "created by the other machine")
	pull := newReconciler("pull", ModePullOnly, WorkspaceReconcilerConfig{PushBaselinePath: filepath.Join(assignmentRoot, "push", "baseline.json")})
	commit(pull, sync(pull, created), created)

	setFile(target, "l1\r\nl2\r\nl3\r\nl4\r\nB5\r\n")
	setFile(filepath.Join(repositoryRoot, "config.txt"), "A1\r\nl2\r\nl3\r\nl4\r\nl5\r\n")
	edited := commitAll(t, repository, "edited by the other machine")
	push = newReconciler("push", ModePushOnly, WorkspaceReconcilerConfig{MergeConcurrentEdits: true, PullBaselinePath: filepath.Join(assignmentRoot, "pull", "baseline.json")})
	prepared := sync(push, edited)
	value, err := os.ReadFile(target)
	if err != nil || string(value) != "A1\r\nl2\r\nl3\r\nl4\r\nB5\r\n" || !prepared.HasChanges {
		t.Fatalf("target = %q, %v, publishes = %t, conflicts = %#v", value, err, prepared.HasChanges, push.Diagnostics().Conflicts)
	}
	if conflicts := push.Diagnostics().Conflicts; len(conflicts) != 0 {
		t.Fatalf("conflicts = %#v", conflicts)
	}
}
