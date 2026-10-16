package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/supportbundle"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelcreatejournal"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelenrollment"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

type cancelAfterWrite struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelAfterWrite) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.cancel()
	return n, err
}

func withTunnelCommandClient(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	old := tunnelClientForCommand
	tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) {
		return api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client()), nil
	}
	t.Cleanup(func() { tunnelClientForCommand = old })
	oldSelector := resolveTunnelSelectorForCommand
	resolveTunnelSelectorForCommand = func(ctx context.Context, client *api.Client, value string) (string, error) {
		if strings.HasPrefix(value, "tun_") && validTunnelCLIResourceID(value) {
			return value, nil
		}
		return resolveTunnelSelector(ctx, client, value)
	}
	t.Cleanup(func() { resolveTunnelSelectorForCommand = oldSelector })
	oldKey := newTunnelIdempotencyKey
	newTunnelIdempotencyKey = func() (string, error) { return "idem_test", nil }
	t.Cleanup(func() { newTunnelIdempotencyKey = oldKey })
	workflowRoot := t.TempDir()
	oldWorkflow := beginTunnelCreateWorkflowForCommand
	beginTunnelCreateWorkflowForCommand = func(ctx context.Context, request tunnelCreateWorkflowRequest) (tunnelCreateWorkflow, error) {
		nameDigest := sha256.Sum256([]byte(request.Name))
		return tunnelcreatejournal.Begin(ctx, tunnelcreatejournal.Config{StateRoot: workflowRoot, HostID: "host_1", NameDigest: hex.EncodeToString(nameDigest[:]), RequestDigest: request.RequestDigest, DomainCount: request.DomainCount, ExpiresAt: request.ExpiresAt, NewKey: tunnelKey})
	}
	t.Cleanup(func() { beginTunnelCreateWorkflowForCommand = oldWorkflow })
	oldRuntime := tunnelConnectorAddRuntime
	tunnelConnectorAddRuntime = func(command *cobra.Command, tunnelID string) error {
		ready := time.Unix(3, 0).UTC()
		projection := tunnelenrollment.Projection{Schema: tunnelenrollment.Schema, Kind: "tunnel_connector", TunnelID: tunnelID, HostID: "host_1", ConnectorID: "connector_1", OperationID: "operation_connector_1", State: "ready", CredentialReference: "protected-file://paperboat/connector_1", CredentialGeneration: 1, ReadyAt: &ready}
		return tunnelOutput(command, projection, "connector ready")
	}
	t.Cleanup(func() { tunnelConnectorAddRuntime = oldRuntime })
}

func validCommandTunnel() api.Tunnel {
	return api.Tunnel{Schema: api.TunnelV1Schema, Kind: "tunnel", ID: "tun_1", AccountID: "acc_1", Generation: 4, ETag: `"tunnel:tun_1:4"`, Name: "demo", AccessMode: "private", DesiredState: "active", StableEndpointID: "endpoint_123e4567-e89b-42d3-a456-426614174000", StableEndpoint: "https://123e4567-e89b-42d3-a456-426614174000.tunnels.example.test", CreatedByHostID: "host_1", CreatedByActorID: "user_1", SummaryCode: "ready", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC()}
}

func validCommandOperation(kind, id string) api.TunnelOperation {
	return api.TunnelOperation{Schema: api.TunnelV1Schema, Kind: "operation", ID: "operation_1", ResourceKind: kind, ResourceID: id, Phase: "connecting", State: "running", Progress: 1, CorrelationID: "correlation_1", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
}

func TestTunnelCommandSurfaceIsExplicitAndSecretSafe(t *testing.T) {
	root := tunnelCobraCommandV1()
	for _, name := range []string{"create", "list", "show", "pause", "resume", "delete", "status", "doctor", "logs", "route", "domain", "connector", "credentials"} {
		if child, _, _ := root.Find([]string{name}); child == nil || child == root {
			t.Fatalf("missing %s", name)
		}
	}
	connector, _, _ := root.Find([]string{"connector"})
	for _, name := range []string{"list", "add", "drain", "revoke"} {
		if child, _, _ := connector.Find([]string{name}); child == nil || child == connector {
			t.Fatalf("missing connector %s", name)
		}
	}
	for _, path := range [][]string{{"route", "add"}, {"route", "update"}, {"route", "remove"}, {"domain", "add"}, {"domain", "verify"}, {"domain", "remove"}, {"domain", "instructions"}, {"credentials", "rotate"}} {
		if child, _, _ := root.Find(path); child == nil || child == root {
			t.Fatalf("missing %v", path)
		}
	}
	for _, forbidden := range []string{"enroll", "enrollment", "token", "secret"} {
		if child, _, _ := connector.Find([]string{forbidden}); child != connector {
			t.Fatalf("unsafe connector command %q exists", forbidden)
		}
	}
	wantRoot := []string{"connector", "create", "credentials", "delete", "doctor", "domain", "inspect", "list", "logs", "pause", "policy", "rename", "replay", "resume", "route", "show", "status", "stop"}
	var gotRoot []string
	for _, child := range root.Commands() {
		if !child.Hidden {
			gotRoot = append(gotRoot, child.Name())
		}
	}
	sort.Strings(gotRoot)
	if strings.Join(gotRoot, ",") != strings.Join(wantRoot, ",") {
		t.Fatalf("tunnel commands=%v want=%v", gotRoot, wantRoot)
	}
	for _, expected := range []struct {
		path []string
		want []string
	}{
		{path: []string{"route"}, want: []string{"add", "list", "remove", "update"}},
		{path: []string{"domain"}, want: []string{"add", "instructions", "list", "remove", "verify"}},
		{path: []string{"connector"}, want: []string{"add", "drain", "list", "revoke"}},
		{path: []string{"credentials"}, want: []string{"rotate"}},
	} {
		parent, _, _ := root.Find(expected.path)
		var got []string
		for _, child := range parent.Commands() {
			if !child.Hidden {
				got = append(got, child.Name())
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(expected.want, ",") {
			t.Fatalf("%v commands=%v want=%v", expected.path, got, expected.want)
		}
	}
	for _, path := range [][]string{{"route", "get"}, {"connector", "get"}} {
		child, _, _ := root.Find(path)
		if child != nil && child.Name() == path[len(path)-1] {
			t.Fatalf("undocumented command is exposed: %v", path)
		}
	}
}

func TestTunnelAndAccessAreRegisteredExactlyOnce(t *testing.T) {
	root := newRootCommand()
	counts := map[string]int{}
	for _, child := range root.Commands() {
		counts[child.Name()]++
	}
	if counts["tunnel"] != 1 || counts["access"] != 1 {
		t.Fatalf("root counts=%v", counts)
	}
	access, _, err := root.Find([]string{"access"})
	if err != nil {
		t.Fatal(err)
	}
	tunnelCount := 0
	for _, child := range access.Commands() {
		if child.Name() == "tunnel" {
			tunnelCount++
		}
	}
	if tunnelCount != 1 {
		t.Fatalf("access tunnel count=%d", tunnelCount)
	}
}

func TestTunnelHelpIsCanonicalAndSecretSafe(t *testing.T) {
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"create", "--help"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	help := output.String()
	for _, required := range []string{"create <name>", "--port", "--from", "--domain", "--private", "--duration", "--wait", "--timeout", "--json"} {
		if !strings.Contains(help, required) {
			t.Fatalf("help missing %q: %s", required, help)
		}
	}
	for _, forbidden := range []string{"enrollment-token", "credential-secret", "private-key"} {
		if strings.Contains(strings.ToLower(help), forbidden) {
			t.Fatalf("unsafe help: %s", help)
		}
	}
}

func TestTunnelDoctorHelpExplainsLocalPreviewAndExplicitWrite(t *testing.T) {
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"doctor", "--help"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	help := output.String()
	for _, required := range []string{"doctor <tunnel>", "--bundle", "--write-bundle", "--bundle-max-bytes", "absolute path", "exact previewed bundle"} {
		if !strings.Contains(help, required) {
			t.Fatalf("doctor help missing %q: %s", required, help)
		}
	}
	if strings.Contains(strings.ToLower(help), "upload") {
		t.Fatalf("doctor help implies upload: %s", help)
	}
}

func TestTunnelCompletionUsesTheCanonicalSurface(t *testing.T) {
	var output bytes.Buffer
	root := newRootCommand()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"__complete", "tunnel", ""})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	completion := output.String()
	for _, required := range []string{"create\t", "list\t", "route\t", "domain\t", "connector\t", "credentials\t"} {
		if !strings.Contains(completion, required) {
			t.Fatalf("completion missing %q: %s", required, completion)
		}
	}
	for _, forbidden := range []string{"enroll\t", "token\t"} {
		if strings.Contains(completion, forbidden) {
			t.Fatalf("unsafe completion %q: %s", forbidden, completion)
		}
	}
}

func TestConnectorAddUsesInjectedRuntimeAndFailsTypedWhenAbsent(t *testing.T) {
	withTunnelCommandClient(t, func(_ http.ResponseWriter, request *http.Request) {
		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
	})
	old := tunnelConnectorAddRuntime
	t.Cleanup(func() { tunnelConnectorAddRuntime = old })
	tunnelConnectorAddRuntime = nil
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"connector", "add", "tun_1"})
	if err := command.Execute(); !errors.Is(err, ErrTunnelConnectorAddRequiresRuntime) {
		t.Fatalf("error=%v", err)
	}
	called := ""
	tunnelConnectorAddRuntime = func(_ *cobra.Command, tunnel string) error { called = tunnel; return nil }
	command = tunnelCobraCommandV1()
	command.SetArgs([]string{"connector", "add", "tun_1"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if called != "tun_1" {
		t.Fatalf("called=%q", called)
	}
}

func TestTunnelCreateValidatesBeforeConstructingClient(t *testing.T) {
	old := tunnelClientForCommand
	t.Cleanup(func() { tunnelClientForCommand = old })
	called := false
	tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) { called = true; return nil, errors.New("must not be called") }
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"create", "bad name", "--port", "80"})
	if err := command.Execute(); err == nil {
		t.Fatal("expected validation error")
	}
	if called {
		t.Fatal("client was constructed before validation")
	}
}

func TestTunnelLogsFollowIsCancellationSafeAndRedacted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requests := 0
	withTunnelCommandClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		entry := api.TunnelLogEntry{Schema: api.TunnelV1Schema, Kind: "log_entry", ID: "log_1", TunnelID: "tun_1", Level: "info", Component: "edge", Code: "ready", Message: "Bearer hidden", Metadata: map[string]any{"access_token": "hidden"}, OccurredAt: time.Unix(1, 0).UTC(), Cursor: "cur_1"}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelLogPage{Items: []api.TunnelLogEntry{entry}}})
	})
	output := &cancelAfterWrite{cancel: cancel}
	command := tunnelCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(output)
	command.SetArgs([]string{"logs", "tun_1", "--follow", "--interval", "250ms", "--json"})
	err := command.Execute()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
	if strings.Contains(output.String(), "hidden") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("output=%s", output.String())
	}
}

func TestTunnelHumanLogsExposeBoundedContinuation(t *testing.T) {
	requests := 0
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("limit") != "1" {
			t.Errorf("limit=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelLogPage{Items: []api.TunnelLogEntry{{Schema: api.TunnelV1Schema, Kind: "log_entry", ID: "log_1", TunnelID: "tun_1", Level: "info", Component: "edge", Code: "ready", Message: "ready", OccurredAt: time.Unix(1, 0).UTC(), Cursor: "cur_1"}}, NextCursor: "cur_1"}})
	})
	command := tunnelCobraCommandV1()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"logs", "tun_1", "--limit", "1"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !strings.Contains(output.String(), "logs tun_1 --cursor cur_1 --limit 1") {
		t.Fatalf("requests=%d output=%q", requests, output.String())
	}
}

func TestTunnelLogsRejectsInvalidBoundsBeforeRequest(t *testing.T) {
	requests := 0
	withTunnelCommandClient(t, func(http.ResponseWriter, *http.Request) { requests++ })
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"logs", "tun_1", "--limit", "201"})
	if err := command.Execute(); err == nil {
		t.Fatal("expected limit error")
	}
	if requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}

type tunnelCommandFailingWriter struct{}

func (tunnelCommandFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output failed")
}

func TestTunnelHumanLogOutputReturnsWriterError(t *testing.T) {
	command := tunnelCobraCommandV1()
	command.SetOut(tunnelCommandFailingWriter{})
	command.SetArgs([]string{"logs", "tun_1"})
	withTunnelCommandClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelLogPage{
			Items: []api.TunnelLogEntry{{Schema: api.TunnelV1Schema, Kind: "log_entry", ID: "log_1", TunnelID: "tun_1", OccurredAt: time.Unix(1, 0).UTC(), Level: "info", Component: "edge", Code: "ready", Message: "ready"}},
		}})
	})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "output failed") {
		t.Fatalf("error=%v", err)
	}
}

func TestNormalizeTunnelCLIPathPrefixRejectsEmbeddedWildcards(t *testing.T) {
	for _, value := range []string{"/api/*/v1", "/api**", "/api*tail"} {
		if _, err := normalizeTunnelCLIPathPrefix(value); err == nil {
			t.Fatalf("path %q was accepted", value)
		}
	}
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "/", want: "/"},
		{input: "/api/*", want: "/api/"},
		{input: "/api/v1", want: "/api/v1"},
	} {
		if got, err := normalizeTunnelCLIPathPrefix(test.input); err != nil || got != test.want {
			t.Fatalf("normalize(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
}

func TestTunnelDoctorPrintsDeterministicHealth(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/tunnels" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelPage{Items: []api.Tunnel{validCommandTunnel()}}})
			return
		}
		d := api.TunnelHealthDimension{Status: "healthy", Code: "ready"}
		health := api.TunnelHealth{Schema: api.TunnelV1Schema, Kind: "health", ResourceKind: "tunnel", ResourceID: "tun_1", OverallCode: "ready", Summary: "Tunnel is ready.", RepairAction: "none", Since: time.Unix(1, 0).UTC(), Dimensions: api.TunnelHealthDimensions{Service: d, Edge: d, Config: d, Route: d, Origin: d, DNS: d, Certificate: d, Access: d, Update: d}}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": health})
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"doctor", "demo"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "Tunnel tun_1: ready\nTunnel is ready.\n" +
		"service      healthy (ready)\nedge         healthy (ready)\nconfig       healthy (ready)\n" +
		"route        healthy (ready)\norigin       healthy (ready)\ndns          healthy (ready)\n" +
		"certificate  healthy (ready)\naccess       healthy (ready)\nupdate       healthy (ready)\nRepair: none\n"
	if output.String() != want {
		t.Fatalf("output=%q", output.String())
	}
}

func TestTunnelDoctorBundleRequiresPreviewAndExplicitWrite(t *testing.T) {
	oldReport := tunnelDoctorLocalReportForCommand
	oldDNS := tunnelDoctorDNSCheckForCommand
	oldHostDiagnostics := tunnelDoctorHostDiagnosticsForCommand
	t.Cleanup(func() {
		tunnelDoctorLocalReportForCommand = oldReport
		tunnelDoctorDNSCheckForCommand = oldDNS
		tunnelDoctorHostDiagnosticsForCommand = oldHostDiagnostics
	})
	tunnelDoctorLocalReportForCommand = func(context.Context) (localDoctorReport, error) {
		return localDoctorReport{
			HostRuntime: "ready", IdentityState: "valid", CredentialState: "valid",
			StateRoot: "/Users/alice/private", MachineID: "machine_private_1", InboxPath: "/Users/alice/inbox",
			RecoveryActions: []string{"authorization=doctor-private-secret"},
		}, nil
	}
	tunnelDoctorDNSCheckForCommand = func(context.Context) bool { return true }
	tunnelDoctorHostDiagnosticsForCommand = func(context.Context) (tunnelHostDiagnostics, error) {
		return tunnelHostDiagnostics{}, errors.New("typed host diagnostics unavailable")
	}
	dimension := api.TunnelHealthDimension{Status: "healthy", Code: "ready"}
	health := api.TunnelHealth{Schema: api.TunnelV1Schema, Kind: "health", ResourceKind: "tunnel", ResourceID: "tun_1", OverallCode: "ready", Summary: "Tunnel is ready.", RepairAction: "none", Since: time.Unix(1, 0).UTC(), Dimensions: api.TunnelHealthDimensions{Service: dimension, Edge: dimension, Config: dimension, Route: dimension, Origin: dimension, DNS: dimension, Certificate: dimension, Access: dimension, Update: dimension}}
	withTunnelCommandClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/tunnels":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.TunnelPage{Items: []api.Tunnel{validCommandTunnel()}}})
		case "/v1/tunnels/tun_1/status":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": health})
		default:
			t.Fatalf("request=%s", request.URL.String())
		}
	})

	outputPath := filepath.Join(realCommandTempDir(t), "tunnel-support.json")
	previewOutput := executeTunnelDoctorJSON(t, "demo", outputPath, false)
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview wrote output: %v", err)
	}
	if previewOutput.Bundle == nil || previewOutput.Written != nil || previewOutput.Bundle.Path != outputPath || previewOutput.Bundle.SizeBytes == 0 || previewOutput.Bundle.SHA256 == "" || len(previewOutput.Bundle.Manifest.Items) != 2 {
		t.Fatalf("preview = %#v", previewOutput)
	}
	writtenOutput := executeTunnelDoctorJSON(t, "demo", outputPath, true)
	if writtenOutput.Bundle == nil || writtenOutput.Written == nil || writtenOutput.Bundle.SHA256 != previewOutput.Bundle.SHA256 || writtenOutput.Written.SHA256 != previewOutput.Bundle.SHA256 {
		t.Fatalf("written output = %#v", writtenOutput)
	}
	written, err := os.ReadFile(outputPath)
	if err != nil || string(written) != string(mustPreviewBytes(t, health)) {
		t.Fatalf("written bundle changed: bytes=%d err=%v", len(written), err)
	}
	for _, unsafe := range []string{"/Users/alice", "machine_private_1", "doctor-private-secret"} {
		if bytes.Contains(written, []byte(unsafe)) {
			t.Fatalf("private value %q present in bundle", unsafe)
		}
	}
	if !bytes.Contains(written, []byte("unavailable")) {
		t.Fatal("typed unavailable evidence is missing")
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(outputPath)
		if statErr != nil {
			t.Fatalf("bundle stat: %v", statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("bundle mode=%v", info.Mode().Perm())
		}
	}
}

func TestTunnelDoctorBundleValidatesBeforeNetwork(t *testing.T) {
	requests := 0
	withTunnelCommandClient(t, func(http.ResponseWriter, *http.Request) { requests++ })
	for _, arguments := range [][]string{
		{"doctor", "demo", "--write-bundle"},
		{"doctor", "demo", "--bundle", "relative.json"},
		{"doctor", "demo", "--bundle", filepath.Join(realCommandTempDir(t), "bundle.json"), "--bundle-max-bytes", "1024"},
	} {
		command := tunnelCobraCommandV1()
		command.SetArgs(arguments)
		if err := command.Execute(); err == nil {
			t.Fatalf("arguments %v succeeded", arguments)
		}
	}
	if requests != 0 {
		t.Fatalf("requests before validation = %d", requests)
	}
}

func TestTunnelDoctorBundleCollectorHonorsCancellation(t *testing.T) {
	oldReport := tunnelDoctorLocalReportForCommand
	t.Cleanup(func() { tunnelDoctorLocalReportForCommand = oldReport })
	called := false
	tunnelDoctorLocalReportForCommand = func(ctx context.Context) (localDoctorReport, error) {
		called = true
		<-ctx.Done()
		return localDoctorReport{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := buildTunnelDoctorBundle(ctx, api.TunnelHealth{}, tunnelDoctorBundleDefaultBytes)
	var typed *supportbundle.Error
	if !errors.As(err, &typed) || typed.Code != supportbundle.ErrorCanceled || called {
		t.Fatalf("cancellation error=%v called=%t", err, called)
	}
}

func TestResolveTunnelSelectorRejectsIDNameCollision(t *testing.T) {
	canonical := validCommandTunnel()
	canonical.Name = "canonical"
	other := validCommandTunnel()
	other.ID, other.Name, other.ETag = "tun_2", "tun_1", `"tunnel:tun_2:4"`
	client := tunnelSelectorTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/tunnels/tun_1":
			writer.Header().Set("ETag", canonical.ETag)
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": canonical})
		case "/v1/tunnels":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.TunnelPage{Items: []api.Tunnel{other}}})
		default:
			t.Fatalf("request=%s", request.URL.String())
		}
	})
	var ambiguous *TunnelSelectorAmbiguousError
	if _, err := resolveTunnelSelector(t.Context(), client, "tun_1"); !errors.As(err, &ambiguous) {
		t.Fatalf("collision error = %v", err)
	}
}

func TestResolveTunnelSelectorPreservesCanonicalIDWhenUnambiguous(t *testing.T) {
	canonical := validCommandTunnel()
	canonical.Name = "canonical"
	var requests []string
	client := tunnelSelectorTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		switch request.URL.Path {
		case "/v1/tunnels/tun_1":
			writer.Header().Set("ETag", canonical.ETag)
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": canonical})
		case "/v1/tunnels":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.TunnelPage{}})
		default:
			t.Fatalf("request=%s", request.URL.String())
		}
	})
	if id, err := resolveTunnelSelector(t.Context(), client, "tun_1"); err != nil || id != "tun_1" {
		t.Fatalf("canonical selector = %q, %v", id, err)
	}
	if strings.Join(requests, ",") != "/v1/tunnels/tun_1,/v1/tunnels" {
		t.Fatalf("requests=%v", requests)
	}
}

func TestResolveTunnelSelectorPaginatesAndRejectsCursorCycle(t *testing.T) {
	target := validCommandTunnel()
	target.Name = "target"
	client := tunnelSelectorTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		cursor := request.URL.Query().Get("cursor")
		page := api.TunnelPage{NextCursor: "page_2"}
		if cursor == "page_2" {
			page = api.TunnelPage{Items: []api.Tunnel{target}}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": page})
	})
	if id, err := resolveTunnelSelector(t.Context(), client, "target"); err != nil || id != target.ID {
		t.Fatalf("paginated selector = %q, %v", id, err)
	}

	cycleClient := tunnelSelectorTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.TunnelPage{NextCursor: "cycle"}})
	})
	if _, err := resolveTunnelSelector(t.Context(), cycleClient, "missing"); !errors.Is(err, api.ErrUnsafeTunnelResponse) {
		t.Fatalf("cursor cycle error = %v", err)
	}

	pages := 0
	boundedClient := tunnelSelectorTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		pages++
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.TunnelPage{NextCursor: fmt.Sprintf("page_%d", pages)}})
	})
	if _, err := resolveTunnelSelector(t.Context(), boundedClient, "missing"); !errors.Is(err, api.ErrUnsafeTunnelResponse) || pages != 10 {
		t.Fatalf("bounded enumeration error=%v pages=%d", err, pages)
	}
}

func executeTunnelDoctorJSON(t *testing.T, selector, outputPath string, write bool) tunnelDoctorOutput {
	t.Helper()
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	arguments := []string{"doctor", selector, "--bundle", outputPath, "--json"}
	if write {
		arguments = append(arguments, "--write-bundle")
	}
	command.SetArgs(arguments)
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result tunnelDoctorOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode output: %v: %s", err, output.String())
	}
	return result
}

func mustPreviewBytes(t *testing.T, health api.TunnelHealth) []byte {
	t.Helper()
	builder, preview, err := buildTunnelDoctorBundle(t.Context(), health, tunnelDoctorBundleDefaultBytes)
	if err != nil || builder == nil {
		t.Fatalf("build preview: %v", err)
	}
	return preview.Bytes()
}

func tunnelSelectorTestClient(t *testing.T, handler http.HandlerFunc) *api.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
}

func realCommandTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return directory
}

func TestCollectTunnelDoctorLocalReportUsesNoProxyAndStrictHealthJSON(t *testing.T) {
	proxyCalls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls++
		http.Error(w, "proxy must not receive loopback diagnostics", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	tests := []struct {
		name       string
		headers    []string
		body       string
		status     int
		location   string
		wantStatus string
	}{
		{name: "valid", headers: []string{"application/json"}, body: `{"live":true}`, wantStatus: "ready"},
		{name: "wrong content type", headers: []string{"text/plain"}, body: `{"live":true}`, wantStatus: "unhealthy"},
		{name: "multiple content types", headers: []string{"application/json", "application/json"}, body: `{"live":true}`, wantStatus: "unhealthy"},
		{name: "duplicate live", headers: []string{"application/json"}, body: `{"live":true,"live":true}`, wantStatus: "unhealthy"},
		{name: "unknown field", headers: []string{"application/json"}, body: `{"live":true,"secret":false}`, wantStatus: "unhealthy"},
		{name: "trailing data", headers: []string{"application/json"}, body: `{"live":true}{}`, wantStatus: "unhealthy"},
		{name: "oversized body", headers: []string{"application/json"}, body: `{"live":true,"padding":"` + strings.Repeat("x", 64<<10) + `"}`, wantStatus: "unhealthy"},
		{name: "redirect", headers: []string{"application/json"}, body: `{"live":true}`, status: http.StatusFound, location: "/other", wantStatus: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, value := range test.headers {
					w.Header().Add("Content-Type", value)
				}
				if test.location != "" {
					w.Header().Set("Location", test.location)
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			stateRoot := t.TempDir()
			runtimeDirectory := filepath.Join(stateRoot, "runtime")
			if err := os.Mkdir(runtimeDirectory, 0o700); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			descriptor := []byte(fmt.Sprintf(`{"schema":"paperboat.worker-local/v1","listen_address":%q}`, strings.TrimPrefix(server.URL, "http://")))
			if err := os.WriteFile(filepath.Join(runtimeDirectory, "worker-local.json"), descriptor, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
			report, err := collectTunnelDoctorLocalReport(t.Context())
			if err != nil || report.HostRuntime != test.wantStatus {
				t.Fatalf("report=%#v error=%v", report, err)
			}
		})
	}
	if proxyCalls != 0 {
		t.Fatalf("proxy calls = %d", proxyCalls)
	}
}
