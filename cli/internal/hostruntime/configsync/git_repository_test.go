package configsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestSanitizedGitErrorKeepsCauseWithoutExposingIt(t *testing.T) {
	cause := fmt.Errorf("private repository path /Users/alice/.ssh/config: %w", syscall.ENOSPC)
	err := sanitizeGitError(cause)
	if !errors.Is(err, ErrGitRepositoryInvalid) || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("sanitized error lost classification or cause: %v", err)
	}
	if strings.Contains(err.Error(), "/Users/alice") {
		t.Fatalf("sanitized error exposed a private path: %q", err)
	}
	fault := errorreport.ProjectFault(context.Background(), configSyncComponent, configSyncOperation, "reconciliation", configSyncFailureCode, err)
	if fault.Errno != int(syscall.ENOSPC) || fault.Cause != "resource_exhausted" {
		t.Fatalf("fault lost filesystem cause: %#v", fault)
	}
}

type staticAccessSource struct{ access RepositoryAccess }

func (s staticAccessSource) RepositoryAccess(context.Context) (RepositoryAccess, error) {
	return s.access, nil
}

type committingReconciler struct{}

func (committingReconciler) Reconcile(_ context.Context, root string, remote RemoteSnapshot) (PreparedPublication, error) {
	path := filepath.Join(root, "dot_config")
	if err := os.WriteFile(path, []byte("plaintext-config"), 0o600); err != nil {
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
	if _, err := worktree.Add("dot_config"); err != nil {
		return PreparedPublication{}, err
	}
	hash, err := worktree.Commit("paperboat config sync", &git.CommitOptions{Author: &object.Signature{Name: "Paperboat", Email: "config@paperboat.invalid", When: time.Unix(1, 0).UTC()}})
	if err != nil {
		return PreparedPublication{}, err
	}
	return PreparedPublication{ExpectedRemoteRevision: remote.Revision, CommitID: hash.String(), HasChanges: true}, nil
}

func TestGitRepositoryFetchPublishAndObserve(t *testing.T) {
	initRepositoryTransports()
	t.Setenv("PATH", "")
	root := t.TempDir()
	barePath := filepath.Join(root, "remote.git")
	if _, err := git.PlainInit(barePath, true); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(root, "seed")
	seed, err := git.PlainInit(seedPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedPath, ".pbinclude"), []byte(".config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree, _ := seed.Worktree()
	if _, err := worktree.Add(".pbinclude"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("initialize config", &git.CommitOptions{Author: &object.Signature{Name: "Paperboat", Email: "config@paperboat.invalid", When: time.Unix(1, 0).UTC()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{barePath}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}

	checkout := filepath.Join(root, "checkout")
	repository, err := NewGitRepository(GitRepositoryConfig{
		Root: checkout, Access: staticAccessSource{RepositoryAccess{
			RepositoryID: "repository", AssignmentID: "assignment", EnvironmentID: "environment", MachineID: "helper",
			CloneURL: barePath, PublishURL: barePath, Branch: "master", Transport: "local",
			Capability: "repository_contents_write",
			ExpiresAt:  time.Now().Add(time.Hour),
		}}, Reconciler: committingReconciler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := repository.Fetch(context.Background())
	if err != nil || remote.Revision == "" {
		t.Fatalf("remote = %#v, %v", remote, err)
	}
	if revision, review, reviewErr := repository.Review(context.Background(), ""); reviewErr != nil || revision != remote.Revision ||
		len(review) != 1 || review[0].Path != ".pbinclude" || review[0].Reason != "added" {
		t.Fatalf("initial review = %q %#v, %v", revision, review, reviewErr)
	}
	prepared, err := repository.Reconcile(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := NewGitRepository(GitRepositoryConfig{
		Root: checkout, Access: staticAccessSource{RepositoryAccess{
			RepositoryID: "repository", AssignmentID: "assignment", EnvironmentID: "environment", MachineID: "helper",
			CloneURL: barePath, PublishURL: barePath, Branch: "master", Transport: "local",
			Capability: "repository_contents_read", ExpiresAt: time.Now().Add(time.Hour),
		}}, Reconciler: committingReconciler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.Publish(context.Background(), prepared, 1); !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("read-only publish error = %v", err)
	}
	result, err := repository.Publish(context.Background(), prepared, 1)
	if err != nil || !result.Landed {
		t.Fatalf("publish = %#v, %v", result, err)
	}
	updated, err := repository.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revision, review, reviewErr := repository.Review(context.Background(), remote.Revision); reviewErr != nil || revision != updated.Revision ||
		len(review) != 1 || review[0].Path != "dot_config" || review[0].Reason != "changed" {
		t.Fatalf("incremental review = %q %#v, %v", revision, review, reviewErr)
	}
	landed, head, err := repository.ObserveCommit(context.Background(), prepared.CommitID)
	if err != nil || !landed || head != prepared.CommitID {
		t.Fatalf("observe = %v %q, %v", landed, head, err)
	}
	if data, err := os.ReadFile(filepath.Join(checkout, ".git", "config")); err != nil || stringContains(string(data), "must-never-be-persisted") {
		t.Fatalf("Git config contains credential or is unreadable: %v", err)
	}
}

func stringContains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
