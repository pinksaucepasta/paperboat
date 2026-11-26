package hostruntimecmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
)

func TestRuntimeFailureMessageNamesPairingOutcome(t *testing.T) {
	for cause, want := range map[error]string{
		bootstrap.ErrPairingExpired:          "pairing expired",
		bootstrap.ErrInstallationUnavailable: "already used",
		bootstrap.ErrPairingDenied:           "was denied",
		bootstrap.ErrResumeTokenChanged:      "pb reset",
	} {
		if got := runtimeFailureMessage(fmt.Errorf("pair: %w", cause)); !strings.Contains(got, want) || !strings.Contains(got, "Add machine") && !strings.Contains(got, "pb reset") {
			t.Fatalf("%v: message=%q", cause, got)
		}
	}
}

func TestRuntimeFailureMessageKeepsElevatedDetailsPrivate(t *testing.T) {
	remote := &elevation.RemoteError{Code: "install_updater", Message: "private-token-and-path"}
	message := runtimeFailureMessage(fmt.Errorf("pair: %w", remote))
	if !strings.Contains(message, "preparing updater state") || strings.Contains(message, "private-token") {
		t.Fatalf("named installation failure = %q", message)
	}
	remote.Code = "unknown-private-code"
	if message := runtimeFailureMessage(remote); strings.Contains(message, "private-token") || strings.Contains(message, "unknown-private-code") {
		t.Fatalf("unknown remote details exposed = %q", message)
	}
	remote.Code = "install_updater"
	if message := runtimeFailureMessage(runtimeFailure("owned primary failure", remote)); message != "owned primary failure" {
		t.Fatalf("remote cleanup relabeled primary = %q", message)
	}
}

func TestPairEnvironmentPendingDoesNotFailPairing(t *testing.T) {
	for _, cause := range []error{errors.New("private ENV delivery failure"), bootstrap.ErrPairingDenied} {
		var out bytes.Buffer
		if err := reportPairEnvironment(&out, cause, "ready"); err != nil {
			t.Fatal(err)
		}
		if out.String() != "Paperboat is ready; managed environment variables are pending: unlock your ENV vault to deliver them\n" {
			t.Fatalf("pending output=%q", out.String())
		}
	}
	var out bytes.Buffer
	if err := reportPairEnvironment(&out, nil, "ready"); err != nil || out.String() != "ready\n" {
		t.Fatalf("ready output=%q err=%v", out.String(), err)
	}
}

func TestPairEnvironmentStageFailureNamesENVWithoutPrivateCause(t *testing.T) {
	err := &installationStageError{Stage: "environment_readiness", Cause: errors.New("private ENV delivery failure")}
	message := runtimeFailureMessage(err)
	if !strings.Contains(message, "managed environment variables") || strings.Contains(message, "internal installation step") || strings.Contains(message, "private ENV") {
		t.Fatalf("ENV failure=%q", message)
	}
}

func TestRuntimeFailureMessageNamesLockedCredentialStore(t *testing.T) {
	err := fmt.Errorf("initialize Paperboat CLI session: %w", errors.Join(config.ErrCredentialStoreUnavailable, errors.New("exit status 36")))
	if got := runtimeFailureMessage(err); !strings.Contains(got, "security unlock-keychain") {
		t.Fatalf("message = %q", got)
	}
}

// a test VM: `pb update` said "its reason is printed above" while the privileged
// step printed only the generic line. An unclassified failure of that step
// must print its cause; classified ones keep their own message.
func TestPrivilegedInstallStepPrintsUnclassifiedCause(t *testing.T) {
	var out bytes.Buffer
	finishRuntimeCommand(t.Context(), "service", &out, errors.New("stage terminal handover: carrier stopped"), 1)
	if !strings.Contains(out.String(), "pb: cause: stage terminal handover: carrier stopped") {
		t.Fatalf("privileged step output=%q", out.String())
	}
	out.Reset()
	finishRuntimeCommand(t.Context(), "doctor", &out, errors.New("private doctor detail"), 1)
	if strings.Contains(out.String(), "private doctor detail") {
		t.Fatalf("non-service command exposed its cause: %q", out.String())
	}
}

// F13-02: `pb login --fresh` printed only the generic runtime failure because bootstrap wrapped the authored step.
func TestRuntimeFailureMessageNamesAWrappedStep(t *testing.T) {
	err := fmt.Errorf("initialize Paperboat CLI session: %w", runtimeFailure("Paperboat could not start its CLI session", errors.New("private cause")))
	if got := runtimeFailureMessage(err); got != "Paperboat could not start its CLI session" {
		t.Fatalf("message = %q", got)
	}
}

// DEP3: a daemon that exited while `pb pair` installed the service ended as "installing system services" with the
// real reason (the privileged step's last "pb: cause:" line) hidden.
func TestPairNamesWhyThePrivilegedInstallStepFailed(t *testing.T) {
	output := "pb: Paperboat could not start\npb: cause: the runtime worker exited without being stopped: exit status 1; last output: relay grant refused\n"
	err := &installationStageError{Stage: "service_install", Cause: &privilegedStepError{Err: errors.New("exit status 1"), Output: output}}
	message := runtimeFailureMessage(err)
	if !strings.Contains(message, "while installing system services: the runtime worker exited without being stopped") || !strings.Contains(message, "relay grant refused") || !strings.Contains(message, "pb doctor") {
		t.Fatalf("message = %q", message)
	}
	silent := &installationStageError{Stage: "service_install", Cause: &privilegedStepError{Err: errors.New("exit status 1")}}
	if got := runtimeFailureMessage(silent); !strings.Contains(got, "while installing system services. Run pb doctor") {
		t.Fatalf("silent step message = %q", got)
	}
}

// A name already taken on the account is enrolled as name-2; the last line names that, not the requested name.
func TestNamedReadyMessageUsesTheServerName(t *testing.T) {
	if got := namedReadyMessage(pairedReadyMessage, "laptop-2"); got != "Ready: this machine is laptop-2. Open it from any device with: pb connect laptop-2" {
		t.Fatal(got)
	}
	if got := namedReadyMessage(pairedReadyMessage, ""); got != pairedReadyMessage {
		t.Fatal(got)
	}
	if degraded := "Paperboat is paired; its local service is still starting. Run `pb status` to see when it is ready."; namedReadyMessage(degraded, "laptop-2") != degraded {
		t.Fatal("a not-ready message was replaced")
	}
}

// Pairing prints its steps in the order they run and ends on the line naming the server's machine name.
// The bootstraps are single long functions, so the order is checked in their source.
func TestPairStepLinesComeInOrderBeforeTheNamedReadyLine(t *testing.T) {
	for file, steps := range map[string][]string{
		"bootstrap_unix.go":    {`"Administrator approval is required`, `"Starting the machine services..."`, `"Connecting to Paperboat..."`, `"Setting up encrypted environment..."`, `namedReadyMessage(paired, material.Alias)`},
		"bootstrap_windows.go": {`"Starting the machine services..."`, `"Setting up encrypted environment..."`, `namedReadyMessage(pairedReadyMessage, material.Alias)`},
	} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		at := 0
		for _, step := range steps {
			next := strings.Index(string(source[at:]), step)
			if next < 0 {
				t.Fatalf("%s: %s missing or out of order", file, step)
			}
			at += next + len(step)
		}
	}
}
