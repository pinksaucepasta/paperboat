//go:build darwin || linux

package updated

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workerupdate"
)

// Manuals change only after the durable runtime commit. Before that boundary,
// rollback leaves the previous manual set untouched. Bundled manual pages never
// block the update: a refresh failure after the commit (for example a
// Homebrew-owned /usr/local/share on macOS that the root-only directory checks
// refuse) is a diagnostic warning, and the next update refreshes them again.
type manualCommitGate struct {
	workerupdate.ActivationGate
	// verify authenticates the candidate executable; its failure stays fatal.
	verify  func(workerupdate.Release) error
	refresh func(context.Context) error
}

func (g *manualCommitGate) Commit(ctx context.Context, request workerupdate.GateRequest) error {
	if err := g.ActivationGate.Commit(ctx, request); err != nil {
		return err
	}
	if g.verify != nil {
		if err := g.verify(request.Candidate); err != nil {
			return err
		}
	}
	warnManualRefresh(g.refresh(ctx))
	return nil
}

// manualWarnings receives refresh failures; the helper's stderr is its log.
var manualWarnings io.Writer = os.Stderr

func warnManualRefresh(err error) {
	if err != nil {
		fmt.Fprintf(manualWarnings, "paperboat-updated: runtime committed; bundled manual pages were not refreshed: %v\n", err)
	}
}

func (s *Service) manualCommitGate(inner workerupdate.ActivationGate) workerupdate.ActivationGate {
	if s.config.RefreshManuals == nil {
		return inner
	}
	// The recovery helper may itself be the old release. Execute only the
	// exact protected candidate bytes selected by the authenticated journal.
	return &manualCommitGate{ActivationGate: inner, verify: func(release workerupdate.Release) error {
		return verifyUnixExecutable(s.config.Binary, release)
	}, refresh: s.config.RefreshManuals}
}
