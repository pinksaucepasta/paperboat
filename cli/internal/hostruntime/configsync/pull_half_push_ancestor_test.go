package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

// A file created and edited on both machines within one poll is never recorded in the pull half's baseline. The push half
// published this machine's version, so a local file still equal to it is unchanged and the remote's merge is applied.
func TestPullHalfTreatsWhatThePushHalfPublishedAsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, local, want string
		conflict          bool
	}{
		{"local is what was published", "ONE\ntwo\nthree\n", "ONE\ntwo\nTHREE\n", false},
		{"local edited after publishing", "own edit\n", "own edit\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			descriptor := testRuntimeDescriptor()
			descriptor.Mode = ModePullOnly
			descriptor.PathRules = []PathRule{{ID: "config", RepositoryPath: "config.txt", LocalPath: target, Kind: "file"}}
			newReconciler := func(name, pushBaseline string) *PlaintextWorkspaceReconciler {
				reconciler, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{
					HomeRoot: homeRoot, StateRoot: filepath.Join(assignmentRoot, name), Descriptor: descriptor, PushBaselinePath: pushBaseline,
				})
				if err != nil {
					t.Fatal(err)
				}
				return reconciler
			}
			setFile := func(path, content string) {
				writeTestFile(t, path, content)
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The push half's baseline: this machine's file as it was when published.
			setFile(filepath.Join(repositoryRoot, "config.txt"), "ONE\ntwo\nthree\n")
			setFile(target, "ONE\ntwo\nthree\n")
			repository, err := git.PlainInit(repositoryRoot, false)
			if err != nil {
				t.Fatal(err)
			}
			published := commitAll(t, repository, "published")
			pushed := newReconciler("push", "")
			prepared, err := pushed.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: published})
			if err != nil {
				t.Fatal(err)
			}
			if err := pushed.PublicationCommitted(context.Background(), prepared, published); err != nil {
				t.Fatal(err)
			}
			// The other machine merged and published; the pull half has no baseline for the file.
			setFile(filepath.Join(repositoryRoot, "config.txt"), "ONE\ntwo\nTHREE\n")
			merged := commitAll(t, repository, "merged by the other machine")
			setFile(target, tc.local)
			pull := newReconciler("pull", filepath.Join(assignmentRoot, "push", "baseline.json"))
			if _, err := pull.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: merged}); err != nil {
				t.Fatal(err)
			}
			value, err := os.ReadFile(target)
			if err != nil || string(value) != tc.want {
				t.Fatalf("target = %q, %v", value, err)
			}
			if conflicts := pull.Diagnostics().Conflicts; (len(conflicts) != 0) != tc.conflict {
				t.Fatalf("conflicts = %#v", conflicts)
			}
		})
	}
}
