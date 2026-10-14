package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestLocalCommandArgumentsFailBeforeAuthenticationWithoutFaultCapture(t *testing.T) {
	for _, args := range [][]string{
		{"machine", "availability", "PRIVATE_MACHINE", "--mode", "PRIVATE_MODE", "--json"},
		{"machine", "capabilities", "PRIVATE_MACHINE", "--json"},
		{"session", "rename", "PRIVATE_MACHINE", "PRIVATE_SESSION", "PRIVATE_NAME", "--json"},
		{"env", "vault", "init", "--json"},
		{"env", "vault", "recover", "--json"},
		{"inbox", "policy", "PRIVATE_INVALID_POLICY", "--json"},
		{"inbox", "requests", "PRIVATE_EXTRA_ARG", "--json"},
		{"config", "set", "ssh-target-port", "PRIVATE_INVALID_PORT", "--json"},
		{"config", "set", "server", "https://PRIVATE_USER:PRIVATE_PASSWORD@example.test", "--json"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			root := t.TempDir()
			isolateCommandCredentialLocation(t, root)
			restore := errorreport.Install(nil)
			defer restore()
			faults := 0
			restoreObserver := errorreport.InstallFaultObserver(func(context.Context, errorreport.Fault) { faults++ })
			defer restoreObserver()
			var stdout, stderr bytes.Buffer
			commandArgs := append([]string{"--config", filepath.Join(root, "config.json")}, args...)
			if code := runWithReporter(t.Context(), commandArgs, &stdout, &stderr, nil); code != 2 {
				t.Fatalf("invalid arguments returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			var result struct {
				OK    bool `json:"ok"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.OK || result.Error.Code != "invalid_invocation" {
				t.Fatalf("expected one usage result: result=%+v decode=%v stdout=%s", result, err, stdout.String())
			}
			if faults != 0 || stderr.Len() != 0 || strings.Contains(stdout.String(), "PRIVATE_") {
				t.Fatalf("argument validation emitted fault or private input: faults=%d stdout=%s stderr=%s", faults, stdout.String(), stderr.String())
			}
		})
	}
}
