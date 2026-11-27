package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/spf13/cobra"
)

// A root step still finishing after pb update --from exits (a rollback past pb's bound) reads the staged update:
// pb removed it on exit and that rollback failed before restoring anything (device check 11-27, linuxpool-7 and
// macpool-1). The staged update is kept for such a step and removed for every other outcome.
func TestUpdateFromKeepsTheStagedUpdateForAStepStillFinishing(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // a kept staging directory is removed with the test
	input := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(input, []byte("candidate bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := installStaged
	t.Cleanup(func() { installStaged = original })
	for _, test := range []struct {
		name string
		err  error
		kept bool
	}{
		{"still finishing", hostruntimecmd.StillFinishingFailure("still restoring the previous version"), true},
		{"failed", errors.New("the new version did not become ready"), false},
		{"installed", nil, false},
		{"interrupted", context.Canceled, true}, // Ctrl-C: a macOS root child may still read it (L46 review)
	} {
		var staged string
		var interrupt func()
		installStaged = func(_ *cobra.Command, _ context.Context, path string, _ bool) error {
			staged = path
			interrupt()
			return test.err
		}
		command := &cobra.Command{}
		command.Flags().String("approve", "", "")
		ctx, cancel := context.WithCancel(context.Background())
		interrupt = func() {
			if test.err == context.Canceled {
				cancel() // the Ctrl-C arrives while the root step runs
			}
		}
		command.SetContext(ctx)
		if err := actionUpdateFrom(command, input, false); !errors.Is(err, test.err) && err != test.err {
			t.Fatalf("%s: error %v, want %v", test.name, err, test.err)
		}
		_, statErr := os.Stat(staged)
		if kept := statErr == nil; kept != test.kept {
			t.Fatalf("%s: staged update kept=%v (%v), want %v", test.name, kept, statErr, test.kept)
		}
		cancel()
	}
}
