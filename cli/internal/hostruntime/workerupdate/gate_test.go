//go:build darwin || linux

package workerupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

type scriptedGate struct {
	mu                                                        sync.Mutex
	candidateErr, drainErr, activeErr, commitErr, rollbackErr error
	candidate, drain, active, commit, rollback                int
}

func (g *scriptedGate) Candidate(context.Context, GateRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.candidate++
	return g.candidateErr
}
func (g *scriptedGate) Drain(ctx context.Context, _ GateRequest) error {
	g.mu.Lock()
	g.drain++
	err := g.drainErr
	g.mu.Unlock()
	if errors.Is(err, context.DeadlineExceeded) {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}
func (g *scriptedGate) Active(context.Context, GateRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active++
	return g.activeErr
}
func (g *scriptedGate) Commit(context.Context, GateRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commit++
	return g.commitErr
}
func (g *scriptedGate) Rollback(context.Context, GateRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollback++
	return g.rollbackErr
}

// A native owner returns the verified prior Source only after restoration. The
// manager then owns gate reconciliation/quarantine, never executable rollback.
func TestPreparedGateFailureKeepsNativeOwnerAndApprovalContracts(t *testing.T) {
	for _, mode := range []string{"old route unhealthy", "drain deadline", "candidate unhealthy", "rollback uncertain"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			failure := errors.New(mode)
			gate := &scriptedGate{}
			switch mode {
			case "old route unhealthy":
				gate.candidateErr = failure
			case "drain deadline":
				gate.drainErr = context.DeadlineExceeded
				f.manager.config.DrainTimeout = time.Millisecond
			case "candidate unhealthy":
				gate.activeErr = failure
			case "rollback uncertain":
				gate.activeErr = failure
				gate.rollbackErr = errors.New("restored route unavailable")
			}
			f.manager.config.Gate = gate
			_, err := activatePrepared(context.Background(), f.manager, f.candidate)
			if err == nil || gate.rollback != 1 || f.manager.ActiveVersion() != f.active.Version || !regularMatches(f.paths.current, f.active.Length, f.active.SHA256) {
				t.Fatalf("failed native owner was discarded: %v", err)
			}
			j, e := updateflow.Load(f.paths.journal)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "rollback uncertain" {
				if !errors.Is(err, ErrBlocked) || j.Stage != updateflow.StageAwaitingApproval || j.ApprovedCandidateID == "" {
					t.Fatal("uncertain gate lost approved recovery", err)
				}
			} else {
				expected := updateflow.FailureDrain
				quarantine := ""
				if mode == "candidate unhealthy" {
					expected = updateflow.FailureHealth
					quarantine = f.candidate.Version
				}
				if j.Stage != updateflow.StageIdle || j.LastFailure != expected || j.CandidateVersion != quarantine {
					t.Fatalf("incorrect outcome: %+v", j)
				}
			}
			if mode == "old route unhealthy" && (gate.drain != 0 || gate.active != 0) {
				t.Fatal("unhealthy existing route admitted cutover")
			}
			if mode == "drain deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("drain deadline cause lost", err)
			}
		})
	}
}

func TestNativeSourceCommitKeepsOriginalApprovalUntilGateAndFinalizerComplete(t *testing.T) {
	for _, mode := range []string{"gate", "finalizer"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			retained := filepath.Join(f.paths.root, "retained-helper")
			originalImage, e := os.ReadFile(f.paths.current)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(retained, originalImage, 0700); e != nil {
				t.Fatal(e)
			}
			f.fetcher.body = append(append([]byte(nil), originalImage...), []byte("distinct native candidate")...)
			f.candidate = release(f.candidate.Version, f.fetcher.body)
			if e = os.WriteFile(filepath.Join(f.paths.root, "installed", "pb"), f.fetcher.body, 0700); e != nil {
				t.Fatal(e)
			}
			f.manager.config.RecoveryExecutable = retained
			original := f.manager.config
			failure := errors.New("post-commit completion pending")
			gate := &scriptedGate{}
			f.manager.config.Gate = gate
			original.Gate = gate
			if mode == "gate" {
				gate.commitErr = failure
			} else {
				f.manager.config.FinalizePrepared = func(context.Context, installsource.Source) error { return failure }
			}
			result, err := activatePrepared(context.Background(), f.manager, f.candidate)
			if !errors.Is(err, failure) || result.Version != f.candidate.Version || f.manager.ActiveVersion() != f.candidate.Version {
				t.Fatalf("committed source hidden or rolled back: %+v %v", result, err)
			}
			j, e := updateflow.Load(f.paths.journal)
			if e != nil || j.Stage != updateflow.StageAwaitingApproval || j.ApprovedCandidateID == "" {
				t.Fatal("original completion approval lost", e)
			}
			gate.commitErr = nil
			// The real helper restarts from its original protected previous baseline;
			// the callback observes its own committed receipt and only revalidates.
			restarted, e := New(original)
			if e != nil {
				t.Fatal(e)
			}
			result, err = restarted.Activate(context.Background(), f.candidate)
			if err != nil || !result.Updated || gate.rollback != 0 || restarted.ActiveVersion() != f.candidate.Version {
				t.Fatalf("post-commit retry changed ownership: %+v %v", result, err)
			}
			j, e = updateflow.Load(f.paths.journal)
			if e != nil || j.Stage != updateflow.StageIdle || j.ApprovedCandidateID != "" {
				t.Fatal("completion receipt not cleared", e)
			}
		})
	}
}
