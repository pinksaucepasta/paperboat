package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
)

func TestCommandAPICauseClassificationAndJSON(t *testing.T) {
	reference := supportref.New()
	for _, fixture := range []struct {
		name        string
		status      int
		body        string
		readFailure bool
		unexpected  bool
	}{
		{name: "valid forbidden", status: 403, body: `{"error":{"code":"team_subscription_required","message":"PRIVATE_RESPONSE"}}`},
		{name: "malformed forbidden", status: 403, body: `{"error":{"code":"team_subscription_required","details":42}}`, unexpected: true},
		{name: "malformed unavailable", status: 503, body: `{"error":{"code":"tunnel_unavailable","details":42}}`, unexpected: true},
		{name: "failed forbidden read", status: 403, body: `{"error":`, readFailure: true, unexpected: true},
		{name: "failed unavailable read", status: 503, body: `{"error":`, readFailure: true, unexpected: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(supportref.Header, reference)
				w.WriteHeader(fixture.status)
				_, _ = io.WriteString(w, fixture.body)
			}))
			defer server.Close()
			client := api.New(server.URL, config.Credential{}, server.Client())
			if fixture.readFailure {
				client = api.New(server.URL, config.Credential{}, &http.Client{Transport: commandResponseTransport{status: fixture.status, reference: reference, body: io.NopCloser(io.MultiReader(strings.NewReader(fixture.body), connectProofReadFailure{}))}})
			}
			_, err := client.Me(t.Context())
			if err == nil {
				t.Fatal("fixture did not fail")
			}
			failure := classifyCommandFailure(err)
			result := classifyCLIJSONFailure(err, failure)
			if failure.supportReference != reference || result.SupportReference != reference {
				t.Fatal("original API reference was lost")
			}
			if fixture.unexpected {
				if failure.kind != commandUnexpected || result.Code != "operation_failed" || result.StateChanged != "unknown" {
					t.Fatal("API owner concealed substantive cause or claimed unchanged state")
				}
				if fixture.readFailure && !errors.Is(err, syscall.EIO) {
					t.Fatal("original read failure was lost")
				}
				if !fixture.readFailure {
					var typeError *json.UnmarshalTypeError
					if !errors.As(err, &typeError) {
						t.Fatal("original decoder type was lost")
					}
				}
			} else if failure.kind != commandRejected || result.Code != "team_subscription_required" || result.StateChanged != false {
				t.Fatal("authoritative refusal lost unchanged-state contract")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "PRIVATE") {
				t.Fatal("response payload escaped into public error")
			}
		})
	}
}

type commandResponseTransport struct {
	status    int
	reference string
	body      io.ReadCloser
}

func (r commandResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	header := make(http.Header)
	header.Set(supportref.Header, r.reference)
	return &http.Response{StatusCode: r.status, Header: header, Body: r.body, Request: request}, nil
}

func TestCommandLocalRejectionAndIndependentFailures(t *testing.T) {
	for _, reason := range []commandRejectionReason{commandRejectDefaultSessionRename, commandRejectDefaultSessionDelete, commandRejectOpenSessionDelete, commandRejectMissingServer, commandRejectNoninteractiveHome} {
		original := commandRejection{reason: reason}
		if failure := classifyCommandFailure(original); failure.kind != commandRejected {
			t.Fatal("finite refusal became exception")
		}
		result := classifyCLIJSONError(original)
		if result.Code != "operation_refused" || result.StateChanged != false || result.Message != original.Error() {
			t.Fatal("finite refusal lost public semantics")
		}
		mixed := errors.Join(original, syscall.EIO)
		if classifyCommandFailure(mixed).kind != commandUnexpected || classifyCLIJSONError(mixed).StateChanged != "unknown" {
			t.Fatal("refusal concealed joined operational failure")
		}
		wrapped := commandRejection{reason: reason, cause: syscall.EIO}
		if !errors.Is(wrapped, syscall.EIO) || classifyCommandFailure(wrapped).kind != commandUnexpected {
			t.Fatal("refusal concealed wrapped original failure")
		}
	}
	if classifyCommandFailure(commandRejection{}).kind != commandUnexpected {
		t.Fatal("unknown reason became expected refusal")
	}
	for _, sentinel := range []error{config.ErrNoCredentials, environmentmanager.ErrVaultPending, environmentmanager.ErrVaultLocked, environmentmanager.ErrVaultChanged, environmentmanager.ErrVaultTeamGrantRequired, environmentmanager.ErrVariableNotConfigured} {
		if classifyCommandFailure(fmtCommandCause{cause: sentinel}).kind != commandRejected {
			t.Fatal("known product state became exception")
		}
		if classifyCommandFailure(errors.Join(sentinel, syscall.EIO)).kind != commandUnexpected {
			t.Fatal("expected leaf concealed independent failure")
		}
	}
	for _, sentinel := range []error{environmentmanager.ErrIntegrity, environmentmanager.ErrAuthorityFork} {
		if classifyCommandFailure(sentinel).kind != commandUnexpected {
			t.Fatal("security fault became expected rejection")
		}
	}
}

type fmtCommandCause struct{ cause error }

func (fmtCommandCause) Error() string   { panic("private cause must not be formatted") }
func (e fmtCommandCause) Unwrap() error { return e.cause }

// The child isolates TLS root initialization and the SDK process owner. It
// uses the production constructor and sends only to task-owned test listeners.
func TestCommandAPIFinalCaptureExactlyOnce(t *testing.T) {
	reference := supportref.New()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(supportref.Header, reference)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"team_subscription_required","message":"PRIVATE_PROVIDER_BODY","details":42}}`)
	}))
	defer control.Close()
	var mu sync.Mutex
	var events [][]byte
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "invalid compression", 400)
				return
			}
			defer gz.Close()
			reader = gz
		}
		scanner := bufio.NewScanner(io.LimitReader(reader, 1<<20))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		if !scanner.Scan() {
			http.Error(w, "missing envelope", 400)
			return
		}
		for scanner.Scan() {
			var item struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(scanner.Bytes(), &item) != nil || !scanner.Scan() {
				http.Error(w, "invalid envelope", 400)
				return
			}
			if item.Type == "event" {
				mu.Lock()
				events = append(events, append([]byte(nil), scanner.Bytes()...))
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	roots := filepath.Join(t.TempDir(), "sdk-test-roots.pem")
	if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: receiver.Certificate().Raw}), 0600); err != nil {
		t.Fatal("could not create owned test trust root")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("could not locate test executable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCommandAPIReporterChild$", "-test.timeout=8s")
	child.Env = append(os.Environ(), "PB_COMMAND_REPORTER_CHILD=1", "PB_COMMAND_REPORTER_CONTROL="+control.URL, "PB_COMMAND_REPORTER_REF="+reference, "SSL_CERT_FILE="+roots, "SSL_CERT_DIR="+filepath.Dir(roots), "PB_SENTRY_ENABLED=true", "PB_SENTRY_DSN=https://public@"+strings.TrimPrefix(receiver.URL, "https://")+"/1", "PB_SENTRY_RELEASE=command-classification-test", "PB_SENTRY_LOGS_ENABLED=false", "PB_SENTRY_METRICS_ENABLED=false", "PB_SENTRY_TRACES_SAMPLE_RATE=0")
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated command reporter fixture failed: %s", output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("final SDK exception events=%d, want 1", len(events))
	}
	var event struct {
		Tags      map[string]string
		Exception []json.RawMessage
	}
	if json.Unmarshal(events[0], &event) != nil || len(event.Exception) != 1 || event.Tags["support_reference"] != reference {
		t.Fatal("actual SDK event lost exception or canonical reference")
	}
	if bytes.Contains(events[0], []byte("PRIVATE_PROVIDER_BODY")) {
		t.Fatal("SDK exception retained provider payload")
	}
}

func TestCommandAPIReporterChild(t *testing.T) {
	if os.Getenv("PB_COMMAND_REPORTER_CHILD") != "1" {
		t.Skip("isolated subprocess fixture")
	}
	reporter := errorreport.FromEnvironment()
	if !reporter.Enabled() {
		t.Fatal("production reporter did not enable")
	}
	restore := errorreport.Install(reporter)
	defer restore()
	old := tunnelClientForCommand
	tunnelClientForCommand = func(command *cobra.Command) (*api.Client, error) {
		client := api.New(os.Getenv("PB_COMMAND_REPORTER_CONTROL"), config.Credential{}, nil)
		_, err := client.Me(command.Context())
		return client, err
	}
	defer func() { tunnelClientForCommand = old }()
	oldSelector := resolveTunnelSelectorForCommand
	resolveTunnelSelectorForCommand = func(context.Context, *api.Client, string) (string, error) { return "tun_test", nil }
	defer func() { resolveTunnelSelectorForCommand = oldSelector }()
	finalFaults := 0
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		if fault.Stage == "command" && fault.Code == "unexpected_cli_failure" {
			finalFaults++
		}
	})
	defer restoreObserver()
	reference := os.Getenv("PB_COMMAND_REPORTER_REF")
	var stdout, stderr bytes.Buffer
	code := runWithReporter(supportref.WithContext(t.Context(), reference), []string{"--no-customization", "tunnel", "status", "tun_test", "--json"}, &stdout, &stderr, reporter)
	reporter.Flush(t.Context())
	if code != 1 || finalFaults != 1 || stderr.Len() != 0 {
		t.Fatalf("final command status=%d final_faults=%d stderr_bytes=%d", code, finalFaults, stderr.Len())
	}
	decoder := json.NewDecoder(&stdout)
	var result cliJSONEnvelope
	if decoder.Decode(&result) != nil || result.Error == nil || result.Error.SupportReference != reference || result.Error.StateChanged != "unknown" {
		t.Fatal("single JSON result lost original failure/reference")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		t.Fatal("command emitted duplicate JSON results")
	}
}

// Cause-bearing error values need not be comparable. Public projection must
// use the finite owner's fields without comparing the original error value.
type commandUncomparableCause map[string]bool

func (commandUncomparableCause) Error() string { panic("private cause must not be formatted") }
func (commandUncomparableCause) Unwrap() error { return config.ErrNoCredentials }
func TestCommandLocalRejectionWithUncomparableCause(t *testing.T) {
	original := commandRejection{reason: commandRejectMissingServer, cause: commandUncomparableCause{"owned": true}}
	result := classifyCLIJSONError(original)
	if result.Code != "operation_refused" || result.StateChanged != "unknown" {
		t.Fatal("cause-bearing refusal lost bounded public projection")
	}
}

func TestCommandGenericENVFailureInheritsCause(t *testing.T) {
	cycle := &connectProofError{}
	cycle.cause = cycle
	for _, test := range []struct {
		name  string
		cause error
		kind  commandFailureKind
		state any
		code  string
	}{
		{"locked", environmentmanager.ErrVaultLocked, commandRejected, false, "env_operation_failed"},
		{"pending", environmentmanager.ErrVaultPending, commandRejected, false, "env_operation_failed"},
		{"usage", localArgumentError("Select a valid ENV scope."), commandUsage, false, "invalid_invocation"},
		{"mixed IO", errors.Join(environmentmanager.ErrVaultLocked, syscall.EIO), commandUnexpected, "unknown", "operation_failed"},
		{"integrity", environmentmanager.ErrIntegrity, commandUnexpected, "unknown", "operation_failed"},
		{"cycle", cycle, commandUnexpected, "unknown", "operation_failed"},
		{"missing cause", nil, commandUnexpected, "unknown", "operation_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const message = "Selected ENV action could not finish; review its requirements and retry."
			original := envCommandError(test.cause, message)
			failure := classifyCommandFailure(original)
			result := classifyCLIJSONFailure(original, failure)
			if failure.kind != test.kind || result.Code != test.code || result.StateChanged != test.state || result.Retryable {
				t.Fatal("generic ENV presentation overrode cause or asserted unsafe state/retry")
			}
			if result.Message != message || userFacingError(original) != message {
				t.Fatal("static producer message was lost")
			}
			if test.name == "mixed IO" && !errors.Is(original, syscall.EIO) {
				t.Fatal("original IO cause was lost")
			}
		})
	}
	// A phase owner remains operational even when the underlying state is an
	// expected vault refusal; its completed or pending work needs recovery.
	if classifyCommandFailure(&envHostRefreshFailure{cause: environmentmanager.ErrVaultLocked}).kind != commandOperational {
		t.Fatal("phase-owned recovery lost its declared operational outcome")
	}
	if classifyCommandFailure(&envHostRefreshFailure{cause: errors.Join(environmentmanager.ErrVaultLocked, syscall.EIO)}).kind != commandUnexpected {
		t.Fatal("phase-owned recovery hid independent IO")
	}
}
