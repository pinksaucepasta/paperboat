package workerupdate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func TestPreparedApprovalSurvivesRestartWithoutExecution(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.manager.Activate(ctx, f.candidate); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unprepared activation: %v", err)
	}
	candidate, err := f.manager.Prepare(ctx, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ID == "" || candidate.SHA256 != f.candidate.SHA256 || candidate.Length != f.candidate.Length {
		t.Fatalf("candidate=%+v", candidate)
	}
	if _, err = f.manager.Activate(ctx, f.candidate); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unapproved activation: %v", err)
	}
	if err = f.manager.Approve(ctx, "wrong"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("wrong approval: %v", err)
	}
	if err = f.manager.Approve(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.PreparedCandidate()
	if err != nil || got != candidate {
		t.Fatalf("candidate=%+v err=%v", got, err)
	}
	j, err := updateflow.Load(f.paths.journal)
	if err != nil || j.Stage != updateflow.StageAwaitingApproval || j.ApprovedCandidateID != candidate.ID {
		t.Fatalf("journal=%+v err=%v", j, err)
	}
	if f.starter.starts != 0 || f.hostd.activations != 0 || !regularMatches(f.paths.current, f.active.Length, f.active.SHA256) {
		t.Fatal("prepare/approve/recover changed running installation")
	}
	result, err := restarted.Activate(ctx, f.candidate)
	if err != nil || !result.Updated {
		t.Fatalf("activation=%+v err=%v", result, err)
	}
	if f.fetcher.fetchCalls != 1 {
		t.Fatalf("artifact downloads=%d", f.fetcher.fetchCalls)
	}
	got, err = restarted.PreparedCandidate()
	if err != nil || got.ID != "" {
		t.Fatalf("completed candidate=%+v err=%v", got, err)
	}
}

func TestPreparedCandidateRejectsChangedBytesAndIdentity(t *testing.T) {
	for _, kind := range []string{"bytes", "identity"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			candidate, err := f.manager.Prepare(ctx, f.candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.manager.Approve(ctx, candidate.ID); err != nil {
				t.Fatal(err)
			}
			release := f.candidate
			if kind == "bytes" {
				if err = os.WriteFile(f.paths.staged, []byte("altered executable"), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err = f.manager.PreparedCandidate(); !errors.Is(err, ErrPreparedCandidate) {
					t.Fatalf("projection: %v", err)
				}
				if err = f.manager.Approve(ctx, candidate.ID); !errors.Is(err, ErrPreparedCandidate) {
					t.Fatalf("approval: %v", err)
				}
			} else {
				release.CanarySamples++
			}
			_, err = f.manager.Activate(ctx, release)
			if !errors.Is(err, ErrPreparedCandidate) && !errors.Is(err, ErrApprovalRequired) {
				t.Fatalf("activation: %v", err)
			}
			if f.starter.starts != 0 || f.hostd.activations != 0 {
				t.Fatal("invalid candidate executed")
			}
		})
	}
}

func TestNativeCommitFailureRecoversWithoutRollback(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	calls := 0
	f.manager.config.ActivateRuntime = func(_ context.Context, version string) (hostdproto.Status, error) {
		calls++
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), Epoch: 82, APIVersion: 1}
		f.hostd.active = status
		return status, nil
	}
	commitErr := errors.New("manual refresh failed")
	f.manager.config.CommitRuntime = func(context.Context, Release) error { return commitErr }
	_, err := activatePrepared(ctx, f.manager, f.candidate)
	if !errors.Is(err, commitErr) {
		t.Fatal(err)
	}
	j, err := updateflow.Load(f.paths.journal)
	if err != nil || j.Stage != updateflow.StageCommitted {
		t.Fatalf("journal=%+v err=%v", j, err)
	}
	if calls != 1 || !regularMatches(f.paths.current, f.candidate.Length, f.candidate.SHA256) {
		t.Fatal("healthy native update rolled back")
	}
	f.manager.config.CommitRuntime = func(_ context.Context, r Release) error {
		if r.Version != f.candidate.Version {
			t.Fatalf("commit release=%+v", r)
		}
		return nil
	}
	if err = f.manager.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || f.manager.ActiveVersion() != f.candidate.Version {
		t.Fatalf("recovery calls=%d active=%s", calls, f.manager.ActiveVersion())
	}
}

func TestCanceledUpdateOperationsDoNoWork(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.Prepare(ctx, f.candidate); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.manager.Approve(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := f.manager.Activate(ctx, f.candidate); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.fetcher.fetchCalls != 0 || f.starter.starts != 0 || f.hostd.activations != 0 {
		t.Fatal("cancelled operation performed update work")
	}
	if _, err := os.Stat(f.paths.staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled operation staged bytes: %v", err)
	}
}
