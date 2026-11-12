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

// X-42: a refused machine name says why (the name rule) instead of "the command arguments are invalid", and still never
// echoes the typed value.
func TestInvalidMachineNameStatesTheRule(t *testing.T) {
	for _, args := range [][]string{{"machine", "rename", "PRIVATE_MACHINE", "rel-x-ünï-PRIVATE"}, {"machine", "add", "--name", "a b PRIVATE"}} {
		root := t.TempDir()
		isolateCommandCredentialLocation(t, root)
		var stdout, stderr bytes.Buffer
		if code := runWithReporter(t.Context(), append([]string{"--config", filepath.Join(root, "config.json")}, args...), &stdout, &stderr, nil); code != 2 {
			t.Fatalf("%v returned %d; stdout=%s stderr=%s", args, code, stdout.String(), stderr.String())
		}
		output := stdout.String() + stderr.String()
		if !strings.Contains(output, "1-63 ASCII letters, digits or hyphens") || strings.Contains(output, "PRIVATE") {
			t.Fatalf("%v: the refusal does not state the rule, or echoes the input: %s", args, output)
		}
	}
}
