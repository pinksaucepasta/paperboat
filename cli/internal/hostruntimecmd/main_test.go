package hostruntimecmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"io"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

func TestRunVersion(t *testing.T) {
	for _, argument := range []string{"version", "--version", "-v"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{argument}, &stdout, &stderr); code != 0 {
			t.Fatalf("run %s exit code = %d, want 0; stderr = %q", argument, code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "pb ") {
			t.Fatalf("%s version output = %q", argument, stdout.String())
		}
	}
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	restoreReporter := errorreport.Install(nil)
	defer restoreReporter()
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	var stderr bytes.Buffer
	if code := Execute(ctx, []string{"version"}, strings.NewReader(""), runtimeFailingOutput{}, &stderr); code != 1 {
		t.Fatalf("failed output code=%d", code)
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatalf("output failure was not retained: %+v", faults)
	}
	if strings.Contains(stderr.String(), "input/output") {
		t.Fatalf("private cause in output: %q", stderr.String())
	}
	var recovered bytes.Buffer
	if code := Execute(ctx, []string{"version"}, strings.NewReader(""), &recovered, io.Discard); code != 0 || !strings.HasPrefix(recovered.String(), "pb ") {
		t.Fatal("fresh version output did not recover")
	}

}

func TestBootstrapPromptsForDashboardCommandInputs(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("Studio machine\n"))
	var output bytes.Buffer
	name := ""
	if err := promptBootstrapValue(reader, &output, "Machine name", &name); err != nil {
		t.Fatal(err)
	}
	if name != "Studio machine" {
		t.Fatalf("name=%q", name)
	}
	if output.String() != "Machine name: " {
		t.Fatalf("prompts=%q", output.String())
	}
}

func TestBootstrapFlagsDoNotConsumePromptInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("unused\n"))
	var output bytes.Buffer
	name := "Studio"
	if err := promptBootstrapValue(reader, &output, "Machine name", &name); err != nil {
		t.Fatal(err)
	}
	if name != "Studio" || output.Len() != 0 {
		t.Fatalf("name=%q prompts=%q", name, output.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run unknown exit code = %d, want 2", code)
	}
	if got := stderr.String(); got != "pb: unknown command\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestRemovedTransitionalCommandsAreUnknown(t *testing.T) {
	for _, arguments := range [][]string{{"enroll", "/tmp/config"}, {"send"}} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr); code != 2 {
			t.Fatalf("arguments=%v code=%d stderr=%q", arguments, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "unknown command") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	}
}

func TestHelpDoesNotExposeStandaloneRuntimeCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "bootstrap") || strings.Contains(stdout.String(), "preview create") || !strings.Contains(stdout.String(), "pb daemon __runtime-host") {
		t.Fatalf("help=%q", stdout.String())
	}
}

func TestRuntimeCommandFenceRetainsIndependentCleanupAfterReportedComponent(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	restoreReporter := errorreport.Install(nil)
	defer restoreReporter()
	ctx := supportref.WithContext(context.Background(), supportref.New())
	runtime, err := hostruntime.NewRuntime(hostruntime.Config{Version: "test", Components: []hostruntime.Component{{Capability: "terminal", Required: true, Service: fenceFailedService{}}}})
	if err != nil {
		t.Fatal(err)
	}
	componentErr := runtime.Start(ctx)
	if !errors.Is(componentErr, context.DeadlineExceeded) {
		t.Fatal("component cause missing")
	}
	var output bytes.Buffer
	componentFaultCount := len(faults)
	if code := finishRuntimeCommand(ctx, "daemon", &output, componentErr, 1); code != 1 || len(faults) != componentFaultCount {
		t.Fatalf("reported component was published twice: code=%d faults=%d", code, len(faults))
	}
	output.Reset()
	if code := finishRuntimeCommand(ctx, "daemon", &output, runtimeFailure("runtime cleanup failed", componentErr, syscall.EIO), 1); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if len(faults) < 2 {
		t.Fatalf("faults=%d", len(faults))
	}
	final := faults[len(faults)-1]
	if final.Cause == "deadline_exceeded" || final.Errno != int(syscall.EIO) || final.SupportReference != supportref.FromContext(ctx) {
		t.Fatalf("independent cleanup was not final-owned: %+v", final)
	}
	if strings.Contains(output.String(), "refused") || strings.Contains(output.String(), "input/output") {
		t.Fatalf("private cause in output: %q", output.String())
	}
	faults = nil
	output.Reset()
	if code := Execute(ctx, []string{"version"}, strings.NewReader(""), &output, io.Discard); code != 0 || len(faults) != 0 {
		t.Fatalf("fresh command code=%d faults=%d", code, len(faults))
	}
}

type fenceFailedService struct{}

func (fenceFailedService) Start(context.Context) error    { return context.DeadlineExceeded }
func (fenceFailedService) Shutdown(context.Context) error { return nil }

type runtimeFailingOutput struct{}

func (runtimeFailingOutput) Write([]byte) (int, error) { return 0, syscall.EIO }
func TestServiceRefusalExitStatusPreservesIndependentFailures(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		failure error
		exit    int
	}{
		{"pre-stop refusal", hostdproto.ErrMaintenanceBusy, 75},
		{"pre-stop wait ran out", &hostinstall.MaintenanceBusyTimeoutError{Cause: context.DeadlineExceeded}, 75},
		{"pre-stop verification", hostinstall.ErrUpdateVerificationFailed, 76},
		{"cold carrier refusal", hostinstall.ErrLegacyCarrierRunning, 77},
		{"wrapped cold carrier refusal", runtimeFailure("safe owner context", hostinstall.ErrLegacyCarrierRunning), 77},
		{"joined cold carrier failure", errors.Join(hostinstall.ErrLegacyCarrierRunning, syscall.EIO), 1},
		{"joined verification", errors.Join(hostinstall.ErrUpdateVerificationFailed, errors.New("independent IO")), 1},
		{"joined failure", errors.Join(hostdproto.ErrMaintenanceBusy, errors.New("independent IO failure")), 1},
		{"wrapped uncertain failure", fmt.Errorf("operation failed: %w", hostdproto.ErrMaintenanceBusy), 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := serviceFailureExitCode(testCase.failure); got != testCase.exit {
				t.Fatalf("exit status=%d want%d", got, testCase.exit)
			}
			if testCase.exit == 77 {
				var output bytes.Buffer
				if code := finishRuntimeCommand(context.Background(), "service", &output, testCase.failure, testCase.exit); code != 77 || !strings.Contains(output.String(), "Finish active sessions and SSH connections") {
					t.Fatal("native admission refusal lost actionable output")
				}
				if code := finishRuntimeCommand(context.Background(), "service", runtimeFailingOutput{}, testCase.failure, testCase.exit); code != 1 {
					t.Fatal("output failure concealed behind refusal exit", code)
				}
			}
		})
	}
}

// A carrier that failed to start under the service manager used to leave only the
// generic sentence in the journal, so the cause needed strace to find.
func TestServiceFailuresLogTheirCauseAndInteractiveOnesDoNot(t *testing.T) {
	failure := runtimeFailure("hostd lifecycle socket could not bind", syscall.EADDRINUSE)
	for _, operation := range []string{"hostd", "worker", "session-owner", "carrier", "updated"} {
		var output bytes.Buffer
		if code := finishRuntimeCommand(context.Background(), operation, &output, failure, 1); code != 1 {
			t.Fatalf("%s exit %d", operation, code)
		}
		if !strings.Contains(output.String(), "pb: cause: ") || !strings.Contains(output.String(), "address already in use") {
			t.Fatalf("%s hid its cause: %q", operation, output.String())
		}
	}
	var output bytes.Buffer
	finishRuntimeCommand(context.Background(), "bootstrap", &output, failure, 1)
	if strings.Contains(output.String(), "cause") || strings.Contains(output.String(), "address already in use") {
		t.Fatalf("interactive command printed internals: %q", output.String())
	}
}
