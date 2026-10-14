package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
)

func TestHomeConsumedFailureKeepsOriginalEvidenceAndMixedFailureVisible(t *testing.T) {
	previous := showHomeText
	t.Cleanup(func() { showHomeText = previous })
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	t.Cleanup(restore)
	command := &cobra.Command{Use: "pb"}
	command.SetContext(supportref.WithContext(context.Background(), supportref.New()))
	views := 0
	showHomeText = func(_ *cobra.Command, _, content string) error {
		views++
		if !strings.Contains(content, supportref.FromContext(command.Context())) {
			t.Fatal("menu omitted its diagnostic support reference")
		}
		if strings.Contains(content, "PRIVATE_COMMAND_PAYLOAD") {
			t.Fatal("menu exposed private error text")
		}
		return nil
	}
	calls := 0
	err := errors.Join(&commandPrivateError{calls: &calls}, syscall.EIO)
	if result := showHomeFailure(command, err); result != nil {
		t.Fatal("handled menu failure escaped")
	}
	if views != 1 || len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != supportref.FromContext(command.Context()) || calls != 0 {
		t.Fatal("consumed menu failure lost original correlated evidence")
	}
	displayed := &homeResultError{Err: err}
	if result := showHomeFailure(command, fmt.Errorf("wrapped: %w", displayed)); result != nil || views != 1 || len(faults) != 1 {
		t.Fatal("already handled result was repeated")
	}
	if result := showHomeFailure(command, errors.Join(displayed, syscall.ENOSPC)); result != nil || views != 2 || len(faults) != 2 {
		t.Fatal("displayed child hid an independent failure")
	}
	if homeFailureAlreadyDisplayed((*homeResultError)(nil)) || homeFailureAlreadyDisplayed(&homeResultError{}) {
		t.Fatal("invalid displayed marker suppressed evidence")
	}
}

func TestHomeViewerFailurePreservesActionCauseForFinalOwner(t *testing.T) {
	previous := showHomeText
	t.Cleanup(func() { showHomeText = previous })
	showHomeText = func(_ *cobra.Command, _, _ string) error { return selector.ErrCanceled }
	command := &cobra.Command{Use: "pb"}
	command.SetContext(context.Background())
	err := showHomeFailure(command, syscall.EIO)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, selector.ErrCanceled) || interactiveCanceled(err) {
		t.Fatal("closing the error viewer discarded the action failure")
	}
}
