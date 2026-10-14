package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestTeardownDiagnosticsDoesNotOpenRemovedStorage(t *testing.T) {
	previous := currentLocalDaemonPaths
	t.Cleanup(func() { currentLocalDaemonPaths = previous })
	currentLocalDaemonPaths = func() (localapi.Paths, error) {
		return localapi.Paths{}, errors.New("removed storage")
	}
	for _, args := range [][]string{
		{"uninstall", "--json"},
		{"--no-customization", "uninstall", "--json"},
		{"--config", "private-config", "reset", "--json"},
		{"__runtime-service", "purge"},
		{"__runtime-service", "purge", "--preserve-cli"},
	} {
		t.Run(args[0]+args[1], func(t *testing.T) {
			var stderr bytes.Buffer
			_, closeDiagnostics := openProcessDiagnostics(t.Context(), args, &errorreport.Reporter{}, &stderr)
			closeDiagnostics()
			if stderr.Len() != 0 {
				t.Fatalf("teardown accessed diagnostic storage: %s", stderr.String())
			}
		})
	}
}
