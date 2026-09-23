//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type manualInnerGate struct {
	gateFixture
	commits int
	err     error
}

func (g *manualInnerGate) Commit(context.Context, workerupdate.GateRequest) error {
	g.commits++
	return g.err
}

func TestManualCommitOrderingRetryAndRollbackPreservation(t *testing.T) {
	ctx := context.Background()
	inner := &manualInnerGate{}
	writes := 0
	failure := errors.New("manual storage unavailable")
	gate := &manualCommitGate{ActivationGate: inner, refresh: func(_ context.Context, r workerupdate.Release) error {
		if inner.commits == 0 || r.Version != "candidate" {
			t.Fatal("manuals refreshed before matching runtime commit")
		}
		writes++
		if writes == 1 {
			return failure
		}
		return nil
	}}
	req := workerupdate.GateRequest{Candidate: workerupdate.Release{Version: "candidate"}}
	if err := gate.Rollback(ctx, req); err != nil || writes != 0 {
		t.Fatal("rollback modified prior manuals", err)
	}
	inner.err = errors.New("runtime commit failed")
	if err := gate.Commit(ctx, req); !errors.Is(err, inner.err) || writes != 0 {
		t.Fatal("manuals modified before commit", err)
	}
	inner.err = nil
	if err := gate.Commit(ctx, req); !errors.Is(err, failure) {
		t.Fatal("refresh failure was hidden", err)
	}
	if err := gate.Commit(ctx, req); err != nil || writes != 2 || inner.rollbacks != 1 {
		t.Fatal("commit retry did not repair manuals", err)
	}
}

func TestManualCommitRejectsUnauthenticatedExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("wrong executable"), 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	s := &Service{config: Config{Binary: path, RefreshManuals: func(context.Context) error { called = true; return nil }}}
	gate := s.manualCommitGate(&manualInnerGate{})
	err := gate.Commit(context.Background(), workerupdate.GateRequest{Candidate: workerupdate.Release{Version: "candidate", Length: 1, SHA256: "invalid"}})
	if err == nil || called {
		t.Fatal("unauthenticated executable was invoked")
	}
}
