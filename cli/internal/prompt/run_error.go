package prompt

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"
)

type promptRunFailure struct {
	message string
	cause   error
}

func (e promptRunFailure) Error() string { return e.message }
func (e promptRunFailure) Unwrap() error { return e.cause }

// canceledBeforeRun avoids starting terminal I/O for work already canceled.
func canceledBeforeRun(ctx context.Context, message string) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	return runFailure(ctx, errors.Join(tea.ErrProgramKilled, ctx.Err()), message)
}

func runFailure(ctx context.Context, err error, message string) error {
	if ctx != nil && ctx.Err() != nil {
		status, cause := ctx.Err(), context.Cause(ctx)
		if teaCancellation(err, status) && (cause == nil || cause == status) {
			return status
		}
		err = errors.Join(err, status, cause)
	}
	return promptRunFailure{message: message, cause: err}
}

// Bubble Tea reports ordinary external cancellation as ErrProgramKilled plus
// the context status. Exact leaf matching keeps a concurrent native failure
// from being mistaken for cancellation just because errors.Is matches.
func teaCancellation(err, status error) bool {
	if err == status {
		return true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return false
	}
	causes := joined.Unwrap()
	return len(causes) == 2 && causes[0] == tea.ErrProgramKilled && causes[1] == status
}
