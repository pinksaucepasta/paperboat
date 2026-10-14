package configsync

import (
	"context"
	"errors"
	"sync"
)

// SplitRepository sequences an independently authorized pull workspace and
// push workspace. Pull application completes first; publication is then
// prepared from the push repository's own head, so histories are never
// grafted or force-pushed when the targets differ.
type SplitRepository struct {
	Pull Repository
	Push Repository

	mu         sync.Mutex
	pullRemote RemoteSnapshot
}

func (r *SplitRepository) Fetch(ctx context.Context) (RemoteSnapshot, error) {
	if r == nil || r.Pull == nil || r.Push == nil {
		return RemoteSnapshot{}, ErrGitRepositoryInvalid
	}
	pull, err := r.Pull.Fetch(ctx)
	if err != nil {
		return RemoteSnapshot{}, err
	}
	push, err := r.Push.Fetch(ctx)
	if err != nil {
		return RemoteSnapshot{}, err
	}
	r.mu.Lock()
	r.pullRemote = pull
	r.mu.Unlock()
	return push, nil
}

func (r *SplitRepository) Review(ctx context.Context, base string) (string, []PathSummary, error) {
	if source, ok := r.Pull.(RevisionReviewSource); ok {
		return source.Review(ctx, base)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pullRemote.Revision, nil, nil
}

func (r *SplitRepository) Reconcile(ctx context.Context, pushRemote RemoteSnapshot) (PreparedPublication, error) {
	r.mu.Lock()
	pullRemote := r.pullRemote
	r.mu.Unlock()
	if pullRemote.Revision == "" || pushRemote.Revision == "" {
		return PreparedPublication{}, ErrRemoteRevisionChanged
	}
	pulled, err := r.Pull.Reconcile(ctx, pullRemote)
	if err != nil {
		return PreparedPublication{}, err
	}
	if pulled.HasChanges {
		return PreparedPublication{}, errors.New("pull-only repository prepared a publication")
	}
	if observer, ok := r.Pull.(PublicationObserver); ok {
		if err := observer.PublicationCommitted(ctx, pulled, pullRemote.Revision); err != nil {
			return PreparedPublication{}, err
		}
	}
	return r.Push.Reconcile(ctx, pushRemote)
}

func (r *SplitRepository) Publish(ctx context.Context, prepared PreparedPublication, fencingToken int64) (PublishResult, error) {
	return r.Push.Publish(ctx, prepared, fencingToken)
}
func (r *SplitRepository) ObserveCommit(ctx context.Context, commitID string) (bool, string, error) {
	return r.Push.ObserveCommit(ctx, commitID)
}
func (r *SplitRepository) PublicationCommitted(ctx context.Context, prepared PreparedPublication, revision string) error {
	if observer, ok := r.Push.(PublicationObserver); ok {
		return observer.PublicationCommitted(ctx, prepared, revision)
	}
	return nil
}
func (r *SplitRepository) PublicationPrepared(ctx context.Context, prepared PreparedPublication) error {
	if journal, ok := r.Push.(PublicationJournal); ok {
		return journal.PublicationPrepared(ctx, prepared)
	}
	return nil
}
func (r *SplitRepository) PublicationAborted(ctx context.Context, prepared PreparedPublication) error {
	if journal, ok := r.Push.(PublicationJournal); ok {
		return journal.PublicationAborted(ctx, prepared)
	}
	return nil
}
