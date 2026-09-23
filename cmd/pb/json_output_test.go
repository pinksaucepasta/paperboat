package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestJSONParseFailureIsOneStructuredValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--json", "--definitely-not-a-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	assertCLIJSONFailure(t, stdout.Bytes(), "invalid_invocation", "usage")
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestJSONRejectsByteStreamBeforeCommandExecution(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"ssh", "example", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	assertCLIJSONFailure(t, stdout.Bytes(), "unsupported_output", "usage")
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestPersistentJSONBeforeCommandFeedsExistingJSONHandler(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	code := run(context.Background(), []string{"--json", "--config", configPath, "config", "show"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var value map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &value); err != nil || value["path"] != configPath {
		t.Fatalf("JSON = %q, err=%v", stdout.String(), err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestJSONMachineMutationRequiresExplicitConfirmationBeforeBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"session", "close", "demo", "work", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertCLIJSONFailure(t, stdout.Bytes(), "invalid_invocation", "usage")
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRootJSONReturnsCommandCatalog(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var envelope cliJSONEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Data == nil {
		t.Fatalf("catalog JSON = %q, envelope=%+v err=%v", stdout.String(), envelope, err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestJSONArgumentDetectionStopsAtRemoteArgv(t *testing.T) {
	if jsonArgumentRequested([]string{"exec", "machine", "--", "tool", "--json"}) {
		t.Fatal("remote argv --json was treated as a pb flag")
	}
	if jsonArgumentRequested([]string{"--json", "exec", "machine", "--", "tool", "--json=false"}) != true {
		t.Fatal("pb --json was overridden by a remote argv flag")
	}
	if jsonArgumentRequested([]string{"--json", "--json=false", "status"}) {
		t.Fatal("last explicit --json=false did not win")
	}
}

func TestJSONUninstallRequiresBothConfirmationsWithoutReadingInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"uninstall", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertCLIJSONFailure(t, stdout.Bytes(), "invalid_invocation", "usage")
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestCLIJSONUnknownMutationStateIsExplicit(t *testing.T) {
	value := classifyCLIJSONError(errors.New("backend disconnected"))
	if value.StateChanged != "unknown" || value.OutcomeUncertain {
		t.Fatalf("failure state = %#v", value)
	}
}

func TestJSONVersionHelpAndTerminalShorthand(t *testing.T) {
	for _, args := range [][]string{{"--json", "--version"}, {"status", "--json", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%v) = %d, stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
		var envelope cliJSONEnvelope
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Data == nil {
			t.Fatalf("run(%v) JSON = %q, envelope=%+v err=%v", args, stdout.String(), envelope, err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("run(%v) stderr = %q, want empty", args, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"example", "--json"}, &stdout, &stderr); code != 2 {
		t.Fatalf("terminal shorthand exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertCLIJSONFailure(t, stdout.Bytes(), "unsupported_output", "usage")
}

func TestJSONHelpReturnsRequestedCommandMetadata(t *testing.T) {
	for _, args := range [][]string{{"tunnel", "create", "--json", "--help"}, {"help", "tunnel", "create", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var envelope struct {
				OK   bool                 `json:"ok"`
				Data cliCommandHelpOutput `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || envelope.Data.Command != "pb tunnel create" || envelope.Data.Usage == "" {
				t.Fatalf("help metadata = %+v", envelope)
			}
			foundFrom := false
			for _, flag := range envelope.Data.Flags {
				if flag.Name == "from" && flag.Type == "string" && flag.Description != "" {
					foundFrom = true
				}
			}
			if !foundFrom {
				t.Fatalf("help flags omit --from: %+v", envelope.Data.Flags)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

type failingJSONWriter struct{}

func (failingJSONWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestJSONEarlyOutputWriteFailureReturnsNonzero(t *testing.T) {
	for _, args := range [][]string{{"--json", "--version"}, {"tunnel", "create", "--json", "--help"}} {
		if code := run(context.Background(), args, failingJSONWriter{}, &bytes.Buffer{}); code == 0 {
			t.Fatalf("run(%v) returned success after output failure", args)
		}
	}
}

func TestCLIJSONSupportReferenceMatchesContract(t *testing.T) {
	const reference = "pb-0123456789abcdef0123456789abcdef"
	value := classifyCLIJSONError(&api.APIError{SupportReference: reference})
	if value.SupportReference != reference {
		t.Fatalf("support reference = %q, want %q", value.SupportReference, reference)
	}
	invalid := classifyCLIJSONError(&api.APIError{SupportReference: reference + "0"})
	if invalid.SupportReference != "" {
		t.Fatalf("invalid support reference escaped validation: %q", invalid.SupportReference)
	}

	payload, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contracts", "schemas", "cli", "output.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Error struct {
				Properties map[string]struct {
					Pattern   string `json:"pattern"`
					MaxLength int    `json:"maxLength"`
				} `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(payload, &schema); err != nil {
		t.Fatal(err)
	}
	property, ok := schema.Properties.Error.Properties["support_reference"]
	if !ok || property.Pattern != "^pb-[0-9a-f]{32}$" || property.MaxLength != len(reference) {
		t.Fatalf("support_reference schema = %+v, present=%v", property, ok)
	}
}

func assertCLIJSONFailure(t *testing.T, payload []byte, code, category string) {
	t.Helper()
	var envelope cliJSONEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("invalid JSON %q: %v", payload, err)
	}
	if envelope.SchemaVersion != cliJSONSchemaVersion || envelope.OK || envelope.Error == nil || envelope.Error.Code != code || envelope.Error.Category != category {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
}
