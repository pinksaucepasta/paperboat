//go:build windows

package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// This opt-in native test consumes only metadata for the explicitly authorized
// task fixture and its existing private machine profile. It demonstrates staging
// and embedded SSH read behavior, not account API authentication. No repository
// content, credential values or private keys are printed or published.
func TestWindowsRepositoryReadTaskSSHNoSystemGit(t *testing.T) {
	endpoint, id, branch, credentials := os.Getenv("PB_TEST_REPOSITORY_URL"), os.Getenv("PB_TEST_REPOSITORY_ID"), os.Getenv("PB_TEST_REPOSITORY_BRANCH"), os.Getenv("PB_TEST_REPOSITORY_CREDENTIAL_ROOT")
	if endpoint == "" || id == "" || branch == "" || credentials == "" {
		t.Skip("explicit authorized task SSH fixture metadata required")
	}
	t.Setenv("PATH", "")
	access := RepositoryReadAccess{RepositoryID: id, CloneURL: endpoint, Branch: branch, Transport: "ssh", Capability: "repository_contents_read", ExpiresAt: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	parent := t.TempDir()
	cache := filepath.Join(parent, "private-read-cache")
	_, revision, err := ReadSharedRepositoryConfig(ctx, cache, credentials, access, DefaultSourceConfigLimits())
	if err != nil || !plumbing.IsHash(revision) {
		t.Fatal("native embedded SSH shared read failed", err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 0 {
		t.Fatal("native read retained clone staging")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, _, err := ReadSharedRepositoryConfig(canceled, cache, credentials, access, DefaultSourceConfigLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal("native canceled read did not stop", err)
	}
	entries, err = os.ReadDir(cache)
	if err != nil || len(entries) != 0 {
		t.Fatal("native canceled read retained clone staging")
	}
	// Exercise the runtime's separate working-tree clone staging against this
	// task fixture. Fetch never enters reconciliation or publication.
	cloneParent := filepath.Join(parent, "private-clone-parent")
	if err := EnsureRepositoryCredentialRoot(cloneParent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cloneParent, "checkout")
	repository, err := NewGitRepository(GitRepositoryConfig{Root: root, CredentialRoot: credentials, Access: staticAccessSource{RepositoryAccess{RepositoryID: id, CloneURL: endpoint, Branch: branch, Transport: "ssh", Capability: "repository_contents_read", ExpiresAt: access.ExpiresAt}}, Reconciler: nativeReadOnlyReconciler{}})
	if err != nil {
		t.Fatal("native read-only clone setup failed", err)
	}
	snapshot, err := repository.Fetch(ctx)
	if err != nil || !plumbing.IsHash(snapshot.Revision) {
		t.Fatal("native working-tree clone/fetch failed", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !privateRepositoryCredentialDirectory(root, info) {
		t.Fatal("native working-tree clone root lost current-owner private protection")
	}
	entries, err = os.ReadDir(cloneParent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "checkout" {
		t.Fatal("native working-tree clone retained staging")
	}
	t.Log("authorized task SSH shared read and working-tree clone succeeded; no system Git, reconciliation or publication; staging cleaned")
}

type nativeReadOnlyReconciler struct{}

func (nativeReadOnlyReconciler) Reconcile(context.Context, string, RemoteSnapshot) (PreparedPublication, error) {
	panic("native read-only test entered reconciliation")
}
