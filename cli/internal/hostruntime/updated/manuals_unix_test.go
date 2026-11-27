//go:build darwin || linux

package updated

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workerupdate"
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
	gate := &manualCommitGate{ActivationGate: inner, verify: func(r workerupdate.Release) error {
		if r.Version != "candidate" {
			t.Fatal("wrong candidate verified")
		}
		return nil
	}, refresh: func(context.Context) error {
		if inner.commits == 0 {
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
	var warnings bytes.Buffer
	manualWarnings = &warnings
	defer func() { manualWarnings = os.Stderr }()
	// macpool-5 11-27: a refused manual directory failed every finish until the
	// journal was quarantined as commit_failed. It must only warn.
	if err := gate.Commit(ctx, req); err != nil || writes != 1 || !strings.Contains(warnings.String(), failure.Error()) {
		t.Fatal("manual refresh failure blocked the runtime commit", err, warnings.String())
	}
	if err := gate.Commit(ctx, req); err != nil || writes != 2 || inner.rollbacks != 1 {
		t.Fatal("later commit did not refresh manuals", err)
	}
}

func TestFinalizePreparedCompletesWhenManualRefreshFails(t *testing.T) {
	var warnings bytes.Buffer
	manualWarnings = &warnings
	defer func() { manualWarnings = os.Stderr }()
	root := t.TempDir()
	s := &Service{config: Config{StateRoot: root, RefreshManuals: func(context.Context) error {
		return errors.New("manual refresh remains pending")
	}}}
	if err := s.finalizePrepared(context.Background(), installsource.Source{}); err != nil {
		t.Fatal("manual refresh failure failed the activation finish", err)
	}
	if !strings.Contains(warnings.String(), "manual refresh remains pending") {
		t.Fatal("manual refresh failure was not reported", warnings.String())
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
