package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestPreparationFailuresKeepCauseAndSafeRecovery(t *testing.T) {
	calls := 0
	private := &commandPrivateError{calls: &calls}
	for _, err := range []error{
		commandPreparationFailure{step: prepareSelectedFile, cause: errors.Join(private, syscall.EIO)},
		setupFailure(private),
	} {
		if !errors.Is(err, private) {
			t.Fatal("preparation lost its original cause")
		}
		message := userFacingError(err)
		if message == "" || strings.Contains(message, "PRIVATE_COMMAND_PAYLOAD") {
			t.Fatal("unsafe preparation prose")
		}
		if classifyCLIJSONError(err).StateChanged != "unknown" {
			t.Fatal("unproven preparation mutation claim")
		}
	}
	if calls != 0 {
		t.Fatal("preparation formatted private cause")
	}
}

func TestDeliveredReceiptFailureIsObservedInBothOutputModes(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		var faults []errorreport.Fault
		restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
		ctx := supportref.WithContext(context.Background(), supportref.New())
		var warning bytes.Buffer
		reportDeliveredReceiptFailure(ctx, &warning, jsonOutput, syscall.EIO)
		restore()
		if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != supportref.FromContext(ctx) {
			t.Fatal("receipt failure lost correlated original evidence")
		}
		if jsonOutput && warning.Len() != 0 {
			t.Fatal("receipt warning contaminated JSON output")
		}
		if !jsonOutput && !strings.Contains(warning.String(), "delivery completed") {
			t.Fatal("receipt warning lost successful delivery fact")
		}
	}
}

func TestUpdateProgressCancellationJoinsWorkerAndOriginalFailure(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command.SetContext(ctx)
	var warning bytes.Buffer
	command.SetErr(&warning)
	started, finished := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := updateWithProgress(command, ctx, func(ctx context.Context) (updated.ControlResponse, error) {
			close(started)
			<-ctx.Done()
			defer close(finished)
			return updated.ControlResponse{}, syscall.EIO
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.EIO) {
			t.Fatal("update cancellation lost original worker failure")
		}
		select {
		case <-finished:
		default:
			t.Fatal("update cancellation left worker running")
		}
	case <-time.After(time.Second):
		t.Fatal("update cancellation did not finish")
	}
}

func TestFriendlyCommandErrorPreservesStatusAndReference(t *testing.T) {
	original := &api.APIError{Status: 403, Code: "team_subscription_required", SupportReference: "support_7be995e7-a965-419d-950f-7f64df1e6f5a"}
	if friendlyCommandError(original) != original {
		t.Fatal("friendly presentation replaced the original error")
	}
	value := classifyCLIJSONError(friendlyCommandError(original))
	if value.Code != "team_subscription_required" || value.SupportReference != original.SupportReference || value.StateChanged != false {
		t.Fatal("typed API contract lost")
	}
	setup := classifyCLIJSONError(setupFailure(original))
	if setup.StateChanged != "unknown" || !strings.Contains(setup.Message, "setup") {
		t.Fatal("setup claimed a failed later step undid earlier work")
	}
}
