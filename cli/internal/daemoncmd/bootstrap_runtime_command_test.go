package daemoncmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// pb's root has a persistent --server flag; the container supervisor's pairing step passes the server to a hidden
// command that disables flag parsing. The flag must still reach bootstrap (it arrived empty on the first container).
func TestBootstrapRuntimeCommandKeepsThePersistentServerFlag(t *testing.T) {
	var seen []string
	root := &cobra.Command{Use: "pb", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("server", "", "server override")
	daemon := &cobra.Command{Use: "daemon"}
	hidden := bootstrapRuntimeCommand()
	hidden.RunE = func(command *cobra.Command, args []string) error {
		seen = withServerFlag(command, args)
		return nil
	}
	daemon.AddCommand(hidden)
	root.AddCommand(daemon)
	root.SetArgs([]string{"daemon", "__runtime-bootstrap", "--server", "https://control.example.test", "--state-root", "/s"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, " "); !strings.Contains(got, "--server https://control.example.test") || !strings.Contains(got, "--state-root /s") || strings.Count(got, "--server") != 1 {
		t.Fatalf("bootstrap arguments = %q", got)
	}
}
