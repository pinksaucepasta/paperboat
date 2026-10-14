package hostruntimecmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
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
