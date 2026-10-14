package hostruntimecmd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWindowsPairingPreparationNamesFailedPhaseWithoutPrivateCause(t *testing.T) {
	for _, phase := range []string{"installation executable", "runtime credentials", "machine-control credentials"} {
		t.Run(phase, func(t *testing.T) {
			private := errors.New("private credential and path marker")
			_, err := prepareWindowsBootstrapRuntime(context.Background(), false, true,
				func(context.Context) error {
					if phase == "runtime credentials" {
						return private
					}
					return nil
				},
				func() error { t.Fatal("existing enrollment checkpoint was replayed"); return nil },
				func(context.Context) error {
					if phase == "machine-control credentials" {
						return private
					}
					return nil
				},
				func(context.Context) (string, error) {
					if phase == "installation executable" {
						return "", private
					}
					return "supplied-build", nil
				})
			message := runtimeFailureMessage(err)
			if !errors.Is(err, private) || !strings.Contains(message, phase) || !strings.Contains(message, "retry pairing") || strings.Contains(message, private.Error()) {
				t.Fatalf("phase=%s message=%q cause preserved=%t", phase, message, errors.Is(err, private))
			}
		})
	}
}
