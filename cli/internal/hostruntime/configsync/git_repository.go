package configsync

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"time"
)

var ErrGitRepositoryInvalid = errors.New("invalid config Git repository")

type RepositoryAccessSource interface {
	RepositoryAccess(context.Context) (RepositoryAccess, error)
}

type WorkspaceReconciler interface {
	Reconcile(context.Context, string, RemoteSnapshot) (PreparedPublication, error)
}

type GitRepositoryConfig struct {
	Root           string
	Access         RepositoryAccessSource
	Reconciler     WorkspaceReconciler
	PushTarget     bool
	CredentialRoot string
}

type GitRepository struct {
	root           string
	access         RepositoryAccessSource
	reconciler     WorkspaceReconciler
	pushTarget     bool
	credentialRoot string
	lastRemote     string
	mu             sync.Mutex
}

func NewGitRepository(config GitRepositoryConfig) (*GitRepository, error) {
	if !filepath.IsAbs(config.Root) || filepath.Clean(config.Root) != config.Root ||
		config.Access == nil || config.Reconciler == nil {
		return nil, ErrGitRepositoryInvalid
	}
	parent := filepath.Dir(config.Root)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(ErrGitRepositoryInvalid, err)
	}
	initRepositoryTransports()
	return &GitRepository{credentialRoot: config.CredentialRoot, root: config.Root, access: config.Access, reconciler: config.Reconciler, pushTarget: config.PushTarget}, nil
}

func (r *GitRepository) Fetch(ctx context.Context) (RemoteSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	access, repository, err := r.open(ctx)
	if err != nil {
		return RemoteSnapshot{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, access.ExpiresAt)
	defer cancel()
	opts, err := r.transportAccess(ctx, access)
	if err != nil {
		return RemoteSnapshot{}, err
	}
	defer opts.close()
	err = repository.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin", Auth: opts.auth, CABundle: opts.ca, Prune: true,
		RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + access.Branch + ":refs/remotes/origin/" + access.Branch)},
		Tags:     git.NoTags, Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return RemoteSnapshot{}, sanitizeGitError(err)
	}
	reference, err := repository.Reference(plumbing.NewRemoteReferenceName("origin", access.Branch), true)
	if err != nil || reference.Hash().IsZero() {
		return RemoteSnapshot{}, errors.Join(ErrGitRepositoryInvalid, sanitizeGitError(err))
	}
	r.lastRemote = reference.Hash().String()
	return RemoteSnapshot{Revision: r.lastRemote}, nil
}

func (r *GitRepository) Review(_ context.Context, base string) (string, []PathSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastRemote == "" {
		return "", nil, ErrRemoteRevisionChanged
	}
	repository, err := git.PlainOpen(r.root)
	if err != nil {
		return "", nil, ErrGitRepositoryInvalid
	}
	headCommit, err := repository.CommitObject(plumbing.NewHash(r.lastRemote))
	if err != nil {
		return "", nil, sanitizeGitError(err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return "", nil, sanitizeGitError(err)
	}
	result := make([]PathSummary, 0)
	if plumbing.IsHash(base) {
		baseCommit, baseErr := repository.CommitObject(plumbing.NewHash(base))
		if baseErr == nil {
			baseTree, treeErr := baseCommit.Tree()
			if treeErr != nil {
				return "", nil, sanitizeGitError(treeErr)
			}
			changes, diffErr := object.DiffTree(baseTree, headTree)
			if diffErr != nil {
				return "", nil, sanitizeGitError(diffErr)
			}
			for _, change := range changes {
				path := change.To.Name
				if path == "" {
					path = change.From.Name
				}
				if safeRelativeStatusPath(path) {
					result = append(result, PathSummary{Path: path, Reason: "changed"})
				}
				if len(result) == 1000 {
					break
				}
			}
			return r.lastRemote, result, nil
		}
	}
	files := headTree.Files()
	err = files.ForEach(func(file *object.File) error {
		if safeRelativeStatusPath(file.Name) {
			result = append(result, PathSummary{Path: file.Name, Reason: "added"})
		}
		if len(result) == 1000 {
			return io.EOF
		}
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return "", nil, sanitizeGitError(err)
	}
	return r.lastRemote, result, nil
}

func (r *GitRepository) Reconcile(ctx context.Context, remote RemoteSnapshot) (PreparedPublication, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if remote.Revision == "" {
		return PreparedPublication{}, ErrRemoteRevisionChanged
	}
	return r.reconciler.Reconcile(ctx, r.root, remote)
}

func (r *GitRepository) Publish(ctx context.Context, prepared PreparedPublication, fencingToken int64) (PublishResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !plumbing.IsHash(prepared.CommitID) || !plumbing.IsHash(prepared.ExpectedRemoteRevision) || fencingToken < 1 {
		return PublishResult{}, ErrGitRepositoryInvalid
	}
	access, repository, err := r.open(ctx)
	if err != nil {
		return PublishResult{}, err
	}
	if access.Capability != "repository_contents_write" {
		return PublishResult{}, ErrWritesDisabled
	}
	localReference, err := repository.Reference(plumbing.NewBranchReferenceName(access.Branch), true)
	if err != nil || localReference.Hash().String() != prepared.CommitID {
		return PublishResult{}, ErrGitRepositoryInvalid
	}
	ctx, cancel := context.WithDeadline(ctx, access.ExpiresAt)
	defer cancel()
	if access.Transport == "local" {
		return publishLocal(ctx, repository, access, prepared)
	}
	opts, err := r.transportAccess(ctx, access)
	if err != nil {
		return PublishResult{}, err
	}
	defer opts.close()
	expected := plumbing.NewHash(prepared.ExpectedRemoteRevision)
	reachable, err := commitReachable(repository, expected, localReference.Hash(), 10_000)
	if err != nil || !reachable {
		return PublishResult{}, ErrRemoteRevisionChanged
	}
	err = repository.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin", Auth: opts.auth, CABundle: opts.ca,
		RefSpecs:       []config.RefSpec{config.RefSpec("refs/heads/" + access.Branch + ":refs/heads/" + access.Branch)},
		ForceWithLease: &git.ForceWithLease{RefName: plumbing.NewBranchReferenceName(access.Branch), Hash: expected},
	})
	if err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return PublishResult{RemoteRevision: prepared.CommitID, Landed: true}, nil
		}
		return PublishResult{Uncertain: providerOutcomeUncertain(err)}, sanitizeGitError(err)
	}
	return PublishResult{RemoteRevision: prepared.CommitID, Landed: true}, nil
}

func (r *GitRepository) ObserveCommit(ctx context.Context, commitID string) (bool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !plumbing.IsHash(commitID) {
		return false, "", ErrGitRepositoryInvalid
	}
	access, repository, err := r.open(ctx)
	if err != nil {
		return false, "", err
	}
	ctx, cancel := context.WithDeadline(ctx, access.ExpiresAt)
	defer cancel()
	opts, err := r.transportAccess(ctx, access)
	if err != nil {
		return false, "", err
	}
	defer opts.close()
	err = repository.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin", Auth: opts.auth, CABundle: opts.ca,
		RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + access.Branch + ":refs/remotes/origin/" + access.Branch)},
		Tags:     git.NoTags, Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return false, "", sanitizeGitError(err)
	}
	reference, err := repository.Reference(plumbing.NewRemoteReferenceName("origin", access.Branch), true)
	if err != nil {
		return false, "", sanitizeGitError(err)
	}
	head := reference.Hash()
	landed, err := commitReachable(repository, plumbing.NewHash(commitID), head, 10_000)
	return landed, head.String(), err
}

func (r *GitRepository) PublicationCommitted(ctx context.Context, prepared PreparedPublication, revision string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if observer, ok := r.reconciler.(PublicationObserver); ok {
		return observer.PublicationCommitted(ctx, prepared, revision)
	}
	return nil
}

func (r *GitRepository) PublicationPrepared(ctx context.Context, prepared PreparedPublication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if journal, ok := r.reconciler.(PublicationJournal); ok {
		return journal.PublicationPrepared(ctx, prepared)
	}
	return nil
}

func (r *GitRepository) PublicationAborted(ctx context.Context, prepared PreparedPublication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if journal, ok := r.reconciler.(PublicationJournal); ok {
		return journal.PublicationAborted(ctx, prepared)
	}
	return nil
}

func (r *GitRepository) open(ctx context.Context) (RepositoryAccess, *git.Repository, error) {
	access, err := r.access.RepositoryAccess(ctx)
	if err != nil {
		return RepositoryAccess{}, nil, err
	}
	if !access.ExpiresAt.After(time.Now()) {
		return RepositoryAccess{}, nil, ErrAuthorization
	}
	if access.Branch == "" || plumbing.NewBranchReferenceName(access.Branch).Validate() != nil ||
		(access.Capability != "repository_contents_read" && access.Capability != "repository_contents_write") {
		return RepositoryAccess{}, nil, ErrGitRepositoryInvalid
	}
	repositoryURL := access.CloneURL
	if r.pushTarget {
		repositoryURL = access.PublishURL
	}
	normalized, kind, err := NormalizeRepositoryEndpoint(repositoryURL)
	if err != nil || kind != access.Transport || normalized != repositoryURL {
		return RepositoryAccess{}, nil, ErrGitRepositoryInvalid
	}
	ctx, cancel := context.WithDeadline(ctx, access.ExpiresAt)
	defer cancel()
	if kind == "local" {
		if _, err := openLocalRepositoryContext(ctx, repositoryURL); err != nil {
			return RepositoryAccess{}, nil, err
		}
	}
	opts, err := r.transportAccess(ctx, access)
	if err != nil {
		return RepositoryAccess{}, nil, err
	}
	defer opts.close()
	info, statErr := os.Lstat(r.root)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		staging, err := newPrivateRepositoryTemporaryDirectory(filepath.Dir(r.root), ".pb-config-clone-")
		if err != nil {
			return RepositoryAccess{}, nil, ErrRepositoryUnavailable
		}
		defer os.RemoveAll(staging)
		_, cloneErr := git.PlainCloneContext(ctx, staging, false, &git.CloneOptions{
			URL: repositoryURL, Auth: opts.auth, CABundle: opts.ca,
			RemoteName: "origin", ReferenceName: plumbing.NewBranchReferenceName(access.Branch),
			SingleBranch: true, NoCheckout: false, Tags: git.NoTags,
		})
		if cloneErr != nil {
			return RepositoryAccess{}, nil, sanitizeGitError(cloneErr)
		}
		if err := os.Rename(staging, r.root); err != nil {
			return RepositoryAccess{}, nil, ErrRepositoryUnavailable
		}
		repository, err := git.PlainOpen(r.root)
		if err != nil {
			return RepositoryAccess{}, nil, ErrGitRepositoryInvalid
		}
		return access, repository, nil
	case statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return RepositoryAccess{}, nil, errors.Join(ErrGitRepositoryInvalid, statErr)
	}
	repository, err := git.PlainOpen(r.root)
	if err != nil {
		return RepositoryAccess{}, nil, ErrGitRepositoryInvalid
	}
	remote, err := repository.Remote("origin")
	if err != nil || len(remote.Config().URLs) != 1 || remote.Config().URLs[0] != repositoryURL {
		return RepositoryAccess{}, nil, ErrGitRepositoryInvalid
	}
	return access, repository, nil
}

func commitReachable(repository *git.Repository, want, head plumbing.Hash, limit int) (bool, error) {
	if want == head {
		return true, nil
	}
	seen := make(map[plumbing.Hash]bool)
	queue := []plumbing.Hash{head}
	for len(queue) > 0 && len(seen) < limit {
		hash := queue[0]
		queue = queue[1:]
		if seen[hash] {
			continue
		}
		seen[hash] = true
		commit, err := repository.CommitObject(hash)
		if err != nil {
			return false, sanitizeGitError(err)
		}
		parentErr := commit.Parents().ForEach(func(parent *object.Commit) error {
			if parent.Hash == want {
				queue = nil
				seen[want] = true
				return io.EOF
			}
			queue = append(queue, parent.Hash)
			return nil
		})
		if errors.Is(parentErr, io.EOF) && seen[want] {
			return true, nil
		}
		if parentErr != nil {
			return false, sanitizeGitError(parentErr)
		}
	}
	return seen[want], nil
}

func providerOutcomeUncertain(err error) bool {
	err = gitUnderlyingError(err)
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.As(err, &network)
}

func sanitizeGitError(err error) error {
	err = gitUnderlyingError(err)
	if err == nil {
		return nil
	}
	return &sanitizedGitFailure{cause: err}
}

type sanitizedGitFailure struct{ cause error }

func (*sanitizedGitFailure) Error() string { return "repository operation failed" }
func (e *sanitizedGitFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (*sanitizedGitFailure) Is(target error) bool { return target == ErrGitRepositoryInvalid }

// go-git v5 plumbing errors expose Err without implementing Unwrap.
func gitUnderlyingError(err error) error {
	for range 8 {
		var unexpected *plumbing.UnexpectedError
		var permanent *plumbing.PermanentError
		if errors.As(err, &unexpected) && unexpected.Err != nil {
			err = unexpected.Err
			continue
		}
		if errors.As(err, &permanent) && permanent.Err != nil {
			err = permanent.Err
			continue
		}
		break
	}
	return err
}
