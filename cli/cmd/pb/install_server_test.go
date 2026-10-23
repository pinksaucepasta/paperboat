package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// pb install --server (or pb --server ... install) carries the selected server into the privileged
// install, which learns the local access domain from it (Lane W: staging install read the build default).
func TestInstallCarriesTheSelectedServer(t *testing.T) {
	root := &cobra.Command{Use: "pb"}
	root.PersistentFlags().String("server", "", "")
	install := &cobra.Command{Use: "install", RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(install)
	for _, args := range [][]string{{"--server", "https://api.staging.example.test:8443", "install"}, {"install", "--server", "https://api.staging.example.test:8443/"}} {
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if got := selectedInstallServer(install); got != "https://api.staging.example.test:8443" {
			t.Fatalf("%v: selected server %q", args, got)
		}
		_ = root.PersistentFlags().Set("server", "")
	}
	if got := selectedInstallServer(install); got != "" {
		t.Fatalf("no --server: %q, want the configured server", got)
	}
}
