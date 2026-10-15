package workerupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
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
	if *f.nativeCalls != 0 || f.hostd.activations != 0 || !regularMatches(f.paths.current, f.active.Length, f.active.SHA256) {
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
			if *f.nativeCalls != 0 || f.hostd.activations != 0 {
				t.Fatal("invalid candidate executed")
			}
		})
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
	if f.fetcher.fetchCalls != 0 || *f.nativeCalls != 0 || f.hostd.activations != 0 {
		t.Fatal("cancelled operation performed update work")
	}
	if _, err := os.Stat(f.paths.staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled operation staged bytes: %v", err)
	}
}

func TestApprovedPreparedReleaseRetainsOriginalIdentityAfterReload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var err error
	f.candidate, err = completeJournalRelease(f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.manager.Prepare(ctx, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.PreparedRelease(ctx); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unapproved original: %v", err)
	}
	if err := f.manager.Approve(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(f.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	signed, payload, err := reloaded.PreparedRelease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := PreparedCandidateForRelease(signed)
	if err != nil || restored.ID != candidate.ID || payload.SHA256 != candidate.SHA256 || payload.Length != candidate.Length {
		t.Fatalf("original identity lost: %v", err)
	}
	journal, err := updateflow.Load(f.paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	journal.OwnerMaintenanceGraceSeconds++
	if err := updateflow.Write(f.paths.journal, journal, f.manager.config.OwnerUID, f.manager.config.OwnerGID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reloaded.PreparedRelease(ctx); !errors.Is(err, ErrPreparedCandidate) {
		t.Fatalf("altered policy identity accepted: %v", err)
	}
}

func TestApprovedCandidateCannotBeSilentlySuperseded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	candidate, err := f.manager.Prepare(ctx, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.manager.Approve(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.candidate
	changed.CanarySamples++
	if _, err = f.manager.Prepare(ctx, changed); !errors.Is(err, ErrBlocked) {
		t.Fatal("approved signed policy silently replaced", err)
	}
	after, err := os.ReadFile(f.paths.journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("approved receipt changed", err)
	}
	if got, err := f.manager.PreparedCandidate(); err != nil || got != candidate {
		t.Fatal("original approved identity lost", err)
	}
}

func TestRecoveryRefusesRetiredPhysicalStagesWithoutTouchingSlots(t *testing.T) {
	f := newFixture(t)
	for p, body := range map[string][]byte{f.paths.rollback: []byte("native baseline sentinel"), f.paths.staged: []byte("native candidate sentinel")} {
		if err := os.WriteFile(p, body, 0700); err != nil {
			t.Fatal(err)
		}
	}
	current, err := os.ReadFile(f.paths.current)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []updateflow.Stage{updateflow.StageStaged, updateflow.StageCandidateStarted, updateflow.StageCandidateValidating, updateflow.StageCandidateReady, updateflow.StageDraining, updateflow.StageCutover, updateflow.StageMonitoring, updateflow.StageCommitted, updateflow.StageRollback, updateflow.StageBlocked} {
		t.Run(string(stage), func(t *testing.T) {
			j := withRelease(f.manager.newJournal(), f.candidate, f.paths.staged)
			j.Stage = stage
			j.WorkerID = workerID(f.candidate.Version)
			j.WorkerEpoch = 2
			if err := f.manager.write(j); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(f.paths.journal)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.manager.Recover(context.Background()); !errors.Is(err, ErrBlocked) {
				t.Fatal("staging resumed native physical recovery", err)
			}
			for p, want := range map[string][]byte{f.paths.current: current, f.paths.rollback: []byte("native baseline sentinel"), f.paths.staged: []byte("native candidate sentinel"), f.paths.journal: before} {
				got, e := os.ReadFile(p)
				if e != nil || !bytes.Equal(got, want) {
					t.Fatal("blocked recovery changed owned bytes", p, e)
				}
			}
			if *f.nativeCalls != 0 {
				t.Fatal("legacy stage invoked native activation")
			}
		})
	}
}

func TestRestartedCandidateRequiresExactApprovedStagingBinding(t *testing.T) {
	for _, mode := range []string{"approved", "unapproved", "wrong baseline", "wrong candidate bytes", "symlink receipt", "old physical stage", "wrong source provenance"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			next := append(append([]byte(nil), f.fetcher.body...), []byte("prepared candidate bytes")...)
			f.fetcher.body = next
			f.candidate = release(f.candidate.Version, next)
			candidate, err := f.manager.Prepare(ctx, f.candidate)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "unapproved" {
				if err = f.manager.Approve(ctx, candidate.ID); err != nil {
					t.Fatal(err)
				}
			}
			j, err := updateflow.Load(f.paths.journal)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "wrong source provenance" {
				source, e := installsource.Inspect(f.paths.current, f.active.Version, installsource.Custom)
				if e != nil {
					t.Fatal(e)
				}
				j.ActiveSource = &source
			}
			if mode == "wrong baseline" {
				j.ActiveDigest = f.candidate.SHA256
			}
			if mode == "old physical stage" {
				j.Stage = updateflow.StageCutover
				j.WorkerID = workerID(f.candidate.Version)
				j.WorkerEpoch = 2
			}
			if err = f.manager.write(j); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink receipt" {
				if err = os.Rename(f.paths.journal, f.paths.journal+".target"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(f.paths.journal+".target", f.paths.journal); err != nil {
					t.Fatal(err)
				}
			}
			image := next
			if mode == "wrong candidate bytes" {
				image = append(append([]byte(nil), next...), byte('x'))
			}
			if err = os.WriteFile(f.paths.current, image, 0700); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(f.manager.config)
			if mode == "approved" {
				if err != nil {
					t.Fatal("bound candidate startup rejected", err)
				}
				if err = restarted.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnsafeStorage) {
				t.Fatal("unbound executing candidate accepted", err)
			}
			if *f.nativeCalls != 0 {
				t.Fatal("startup executed a native transition")
			}
		})
	}
}
