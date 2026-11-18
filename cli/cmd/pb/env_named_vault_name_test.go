package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// `pb env set --vault` uses the one ENV name rule (shared/envname): a 129-character or reserved name is refused
// before the value is read or the vault is touched, and a good one reaches the vault.
func TestEnvSetVaultRefusesBadNamesBeforeTouchingTheVault(t *testing.T) {
	reached := errors.New("reached the vault")
	previous := namedVaultSessionFor
	namedVaultSessionFor = func(*cobra.Command) (namedVaultSession, error) { return namedVaultSession{}, reached }
	t.Cleanup(func() { namedVaultSessionFor = previous })
	run := func(args ...string) error {
		root := newRootCommand()
		root.SetIn(strings.NewReader("value"))
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		return root.Execute()
	}
	for _, name := range []string{strings.Repeat("A", 129), "LD_PRELOAD", "1ABC", "A-B"} {
		for _, args := range [][]string{{"env", "set", name, "--vault", "canary-env", "--value-stdin"}} {
			if err := run(args...); err == nil || errors.Is(err, reached) || !strings.Contains(err.Error(), "environment variable name") {
				t.Errorf("%v: err = %v, want a refusal of the name", args[:2], err)
			}
		}
	}
	// Removing a name never needs it to be valid: a vault's odd name can always be taken out.
	if err := run("env", "unset", strings.Repeat("A", 129), "--vault", "canary-env"); !errors.Is(err, reached) {
		t.Fatalf("unset of a 129-character name was refused: %v", err)
	}
	if err := run("env", "set", strings.Repeat("A", 128), "--vault", "canary-env", "--value-stdin"); !errors.Is(err, reached) {
		t.Fatalf("a 128-character name was refused: %v", err)
	}
}
