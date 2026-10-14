package configsync

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type cleanupPrivateCause struct{}

func (*cleanupPrivateCause) Error() string { panic("private cleanup text must not be formatted") }

type ownedCleanupCloser struct {
	entered, release chan struct{}
	calls            atomic.Int32
	cause            error
}

func (c *ownedCleanupCloser) Close() error {
	c.calls.Add(1)
	close(c.entered)
	<-c.release
	return c.cause
}

func TestRepositoryContextCleanupJoinsOriginalCallbackAndRecovers(t *testing.T) {
	cause := &cleanupPrivateCause{}
	underlying := &ownedCleanupCloser{entered: make(chan struct{}), release: make(chan struct{}), cause: cause}
	ctx, cancel := context.WithCancel(context.Background())
	owned := newRepositoryContextCloser(ctx, underlying)
	cancel()
	select {
	case <-underlying.entered:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close owned transport")
	}
	joined := make(chan error, 1)
	go func() { joined <- owned.Close() }()
	select {
	case <-joined:
		t.Fatal("explicit close returned before callback joined")
	case <-time.After(20 * time.Millisecond):
	}
	close(underlying.release)
	select {
	case err := <-joined:
		if !errors.Is(err, cause) {
			t.Fatal("callback cause lost")
		}
	case <-time.After(time.Second):
		t.Fatal("close failed to join")
	}
	if underlying.calls.Load() != 1 || !errors.Is(owned.Close(), cause) {
		t.Fatal("owned close not idempotent")
	}
	access := repositoryTransportAccess{closer: owned}
	err := repositoryCleanupFailure(syscall.EIO, access.close())
	if !errors.Is(err, cause) || !errors.Is(err, syscall.EIO) || err.Error() != "config repository unavailable" {
		t.Fatal("safe cleanup projection lost causes")
	}
	healthy := &ownedCleanupCloser{entered: make(chan struct{}), release: make(chan struct{})}
	close(healthy.release)
	if err := newRepositoryContextCloser(context.Background(), healthy).Close(); err != nil || healthy.calls.Load() != 1 {
		t.Fatal("fresh transport did not recover")
	}
	if (repositoryTransportAccess{}).close() != nil {
		t.Fatal("empty transport cleanup failed")
	}
}

type cleanupJournalRepository struct {
	*fakeRepository
	commits, aborts int
	commitErr       error
}

func (r *cleanupJournalRepository) PublicationPrepared(context.Context, PreparedPublication) error {
	return nil
}
func (r *cleanupJournalRepository) PublicationAborted(context.Context, PreparedPublication) error {
	r.aborts++
	return nil
}
func (r *cleanupJournalRepository) PublicationCommitted(context.Context, PreparedPublication, string) error {
	r.commits++
	return r.commitErr
}

func cleanupPublicationFixture(published PublishResult, publishErr error) (*Publisher, *cleanupJournalRepository, *fakeLeaseAuthority, *[]string) {
	events := []string{}
	repository := &cleanupJournalRepository{fakeRepository: &fakeRepository{events: &events, fetches: []RemoteSnapshot{{Revision: "head"}, {Revision: "head"}, {Revision: "head"}}, prepared: PreparedPublication{ExpectedRemoteRevision: "head", CommitID: "commit", HasChanges: true}, published: published, publishErr: publishErr}}
	authority := &fakeLeaseAuthority{events: &events}
	publisher, _ := NewPublisher(PublisherConfig{Authority: authority, Repository: repository})
	return publisher, repository, authority, &events
}

func TestPublisherKnownCommitSurvivesIndependentCleanupFailures(t *testing.T) {
	for _, phase := range []string{"push_close", "observe_close", "commit_ack", "lease_release", "unresolved_observe"} {
		t.Run(phase, func(t *testing.T) {
			cleanup := &cleanupPrivateCause{}
			publishErr := error(nil)
			published := PublishResult{RemoteRevision: "commit", Landed: true}
			if phase == "push_close" {
				publishErr = repositoryCleanupFailure(nil, cleanup)
			}
			if phase == "observe_close" || phase == "unresolved_observe" {
				published = PublishResult{Uncertain: true}
				publishErr = sanitizeGitError(syscall.ECONNRESET)
			}
			publisher, repository, authority, events := cleanupPublicationFixture(published, publishErr)
			if phase == "observe_close" {
				repository.observed = true
				repository.observedHead = "commit"
				repository.observeErr = repositoryCleanupFailure(nil, cleanup)
			}
			if phase == "unresolved_observe" {
				repository.observeErr = repositoryCleanupFailure(nil, cleanup)
			}
			if phase == "commit_ack" {
				repository.commitErr = repositoryCleanupFailure(nil, cleanup)
			}
			if phase == "lease_release" {
				authority.releaseErr = cleanup
			}
			result, err := publisher.Sync(context.Background(), "head")
			if !errors.Is(err, cleanup) {
				t.Fatal("independent cleanup cause lost")
			}
			pushes := 0
			for _, event := range *events {
				if event == "publish" {
					pushes++
				}
			}
			if pushes != 1 || repository.aborts != 0 {
				t.Fatal("cleanup authorized replay or abort")
			}
			if phase == "unresolved_observe" {
				if result.Landed || !result.Uncertain || !errors.Is(err, syscall.ECONNRESET) || !errors.Is(err, ErrSyncUncertain) || repository.commits != 0 {
					t.Fatal("unresolved observation erased uncertainty or cause")
				}
			} else if !result.Landed || result.Uncertain || result.RemoteRevision != "commit" || repository.commits != 1 {
				t.Fatal("known commit proof lost")
			}
			if phase == "observe_close" && !errors.Is(err, syscall.ECONNRESET) {
				t.Fatal("original publish cause lost alongside observation failure")
			}
		})
	}
}

func TestEngineKnownPublicationCleanupDoesNotReplayAndFreshSyncRecovers(t *testing.T) {
	for _, phase := range []string{"push_close", "commit_ack", "lease_release"} {
		t.Run(phase, func(t *testing.T) {
			cause := &cleanupPrivateCause{}
			publisher, repository, authority, events := cleanupPublicationFixture(PublishResult{RemoteRevision: "commit", Landed: true}, nil)
			if phase == "push_close" {
				repository.publishErr = repositoryCleanupFailure(nil, cause)
			}
			if phase == "commit_ack" {
				repository.commitErr = repositoryCleanupFailure(nil, cause)
			}
			if phase == "lease_release" {
				authority.releaseErr = cause
			}
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal("fixture directory unavailable")
			}
			engine, err := NewEngine(EngineConfig{HomeRoot: home, Descriptor: testEngineDescriptor(3), Syncer: publisher})
			if err != nil {
				t.Fatal("engine fixture invalid")
			}
			engine.dirtySince = time.Now()
			if err := engine.Apply(context.Background()); !errors.Is(err, cause) {
				t.Fatal("cleanup cause lost at engine")
			}
			pushes := 0
			for _, event := range *events {
				if event == "publish" {
					pushes++
				}
			}
			if pushes != 1 || repository.commits != 1 || repository.aborts != 0 {
				t.Fatal("known commit retried or aborted")
			}
			if engine.status.State != "warning" || engine.status.RemoteRevision != "commit" || engine.status.LastSuccessfulAt == nil || !engine.dirtySince.IsZero() {
				t.Fatal("known progress lost or declared healthy")
			}
			for _, action := range engine.status.RecoveryActions {
				if action == "retry" {
					t.Fatal("cleanup advised unsafe publication replay")
				}
			}
			repository.publishErr = nil
			repository.commitErr = nil
			authority.releaseErr = nil
			repository.fetches = []RemoteSnapshot{{Revision: "commit"}}
			repository.prepared = PreparedPublication{ExpectedRemoteRevision: "commit", CommitID: "commit"}
			if err := engine.Apply(context.Background()); err != nil || engine.status.State != "healthy" {
				t.Fatal("fresh sync failed to recover")
			}
			total := 0
			for _, event := range *events {
				if event == "publish" {
					total++
				}
			}
			if total != 1 {
				t.Fatal("fresh reconciliation repeated already-landed push")
			}
		})
	}
}

func TestPublisherAppliedPullAcknowledgementFailureRetainsProgress(t *testing.T) {
	for _, underLease := range []bool{false, true} {
		publisher, repository, _, events := cleanupPublicationFixture(PublishResult{}, nil)
		cause := &cleanupPrivateCause{}
		repository.commitErr = repositoryCleanupFailure(nil, cause)
		if !underLease {
			repository.prepared.HasChanges = false
		} else {
			repository.prepared.HasChanges = true
			// The existing reconciler seam changes only after the initial preparation.
			publisher.repository = &cleanupPullRepository{cleanupJournalRepository: repository}
		}
		result, err := publisher.Sync(context.Background(), "head")
		if !result.Landed || result.RemoteRevision != "head" || !errors.Is(err, cause) || repository.commits != 1 {
			t.Fatal("applied pull progress lost on acknowledgement failure")
		}
		for _, event := range *events {
			if event == "publish" {
				t.Fatal("unchanged pull unexpectedly pushed")
			}
		}
	}
}

type cleanupPullRepository struct {
	*cleanupJournalRepository
	preparations int
}

func (r *cleanupPullRepository) Reconcile(ctx context.Context, remote RemoteSnapshot) (PreparedPublication, error) {
	prepared, err := r.fakeRepository.Reconcile(ctx, remote)
	r.preparations++
	if r.preparations > 1 {
		prepared.HasChanges = false
	}
	return prepared, err
}
