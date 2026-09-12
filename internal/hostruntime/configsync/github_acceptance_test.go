package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type acceptanceReconciler struct{ path string }

func (r acceptanceReconciler) Reconcile(_ context.Context, root string, remote RemoteSnapshot) (PreparedPublication, error) {
	if err := os.WriteFile(filepath.Join(root, r.path), []byte("synthetic-task42-config\n"), 0o600); err != nil {
		return PreparedPublication{}, err
	}
	repository, err := git.PlainOpen(root)
	if err != nil {
		return PreparedPublication{}, err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return PreparedPublication{}, err
	}
	if _, err = worktree.Add(r.path); err != nil {
		return PreparedPublication{}, err
	}
	hash, err := worktree.Commit("Task 42 provider acceptance", &git.CommitOptions{Author: &object.Signature{Name: "Paperboat", Email: "config@paperboat.invalid", When: time.Now().UTC()}})
	return PreparedPublication{ExpectedRemoteRevision: remote.Revision, CommitID: hash.String(), HasChanges: true}, err
}

// TestGitHubProviderAcceptance exercises the production HTTPS Git adapter
// against explicitly disposable private repositories. It is skipped during
// ordinary test runs and never prints or persists the supplied credential.
func TestGitHubProviderAcceptance(t *testing.T) {
	token := os.Getenv("PAPERBOAT_GITHUB_ACCEPTANCE_TOKEN")
	pullURL := os.Getenv("PAPERBOAT_GITHUB_ACCEPTANCE_PULL_URL")
	pushURL := os.Getenv("PAPERBOAT_GITHUB_ACCEPTANCE_PUSH_URL")
	if token == "" || pullURL == "" || pushURL == "" {
		t.Skip("set disposable GitHub acceptance repository coordinates and token")
	}

	access := func(repositoryID, cloneURL, capability string) staticAccessSource {
		return staticAccessSource{RepositoryAccess{
			RepositoryID: repositoryID, AssignmentID: "task42-acceptance", EnvironmentID: "task42-environment",
			MachineID: "task42-machine", CloneURL: cloneURL, PublishURL: cloneURL, Branch: "main",
			Username: "x-access-token", Password: token, Capability: capability, ExpiresAt: time.Now().Add(15 * time.Minute),
		}}
	}
	open := func(name, repositoryID, repositoryURL, capability string) *GitRepository {
		root := filepath.Join(t.TempDir(), name)
		repository, err := NewGitRepository(GitRepositoryConfig{
			Root: root, Access: access(repositoryID, repositoryURL, capability), Reconciler: committingReconciler{}, PushTarget: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return repository
	}
	publish := func(repository *GitRepository) string {
		remote, err := repository.Fetch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := repository.Reconcile(context.Background(), remote)
		if err != nil {
			t.Fatal(err)
		}
		result, err := repository.Publish(context.Background(), prepared, 1)
		if err != nil || !result.Landed {
			t.Fatalf("provider publication landed=%v: %v", result.Landed, err)
		}
		return prepared.CommitID
	}

	// Distinct pull/push targets use separate provider worktrees and heads.
	pull := open("pull", "task42-pull", pullURL, "repository_contents_read")
	if snapshot, err := pull.Fetch(context.Background()); err != nil || snapshot.Revision == "" {
		t.Fatalf("provider pull: %#v, %v", snapshot, err)
	}
	readRemote, err := pull.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	readPrepared, err := pull.Reconcile(context.Background(), readRemote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pull.Publish(context.Background(), readPrepared, 1); !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("read-only capability published: %v", err)
	}
	push := open("push", "task42-push", pushURL, "repository_contents_write")
	distinctCommit := publish(push)
	if landed, head, err := push.ObserveCommit(context.Background(), distinctCommit); err != nil || !landed || head != distinctCommit {
		t.Fatalf("distinct target observation landed=%v head=%q: %v", landed, head, err)
	}

	// A single repository is also valid for both directions without sharing a
	// worktree or credential cache.
	same := open("same-target", "task42-pull", pullURL, "repository_contents_write")
	sameCommit := publish(same)
	if landed, head, err := same.ObserveCommit(context.Background(), sameCommit); err != nil || !landed || head != sameCommit {
		t.Fatalf("same target observation landed=%v head=%q: %v", landed, head, err)
	}
}

func TestGitHubProviderRejectsProtectedPush(t *testing.T) {
	token := os.Getenv("PAPERBOAT_GITHUB_ACCEPTANCE_TOKEN")
	repositoryURL := os.Getenv("PAPERBOAT_GITHUB_ACCEPTANCE_PROTECTED_URL")
	if token == "" || repositoryURL == "" {
		t.Skip("set a disposable protected GitHub acceptance repository and token")
	}
	root := filepath.Join(t.TempDir(), "protected")
	repository, err := NewGitRepository(GitRepositoryConfig{Root: root, PushTarget: true,
		Access: staticAccessSource{RepositoryAccess{
			RepositoryID: "task42-protected", AssignmentID: "task42-acceptance", EnvironmentID: "task42-environment",
			MachineID: "task42-machine", CloneURL: repositoryURL, PublishURL: repositoryURL, Branch: "main",
			Username: "x-access-token", Password: token, Capability: "repository_contents_write", ExpiresAt: time.Now().Add(15 * time.Minute),
		}}, Reconciler: acceptanceReconciler{path: "dot_config_protected"}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := repository.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repository.Reconcile(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repository.Publish(context.Background(), prepared, 1)
	if err == nil || result.Landed {
		t.Fatalf("branch protection accepted direct publication: %#v, %v", result, err)
	}
	landed, head, observeErr := repository.ObserveCommit(context.Background(), prepared.CommitID)
	if observeErr != nil || landed || head != remote.Revision {
		t.Fatalf("rejected commit observation landed=%v head=%q want=%q: %v", landed, head, remote.Revision, observeErr)
	}
}
