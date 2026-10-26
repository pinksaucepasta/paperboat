package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelcreatejournal"
	"github.com/spf13/cobra"
)

const workflowAcceptanceSecret = "workflow-secret-canary-18f7"

type workflowAcceptanceEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *workflowAcceptanceEvents) add(value string) {
	e.mu.Lock()
	e.values = append(e.values, value)
	e.mu.Unlock()
}

func (e *workflowAcceptanceEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

type recordingTunnelCreateWorkflow struct {
	harness *workflowAcceptanceHarness
	session *tunnelcreatejournal.Session
}

func (w *recordingTunnelCreateWorkflow) Snapshot() tunnelcreatejournal.Journal {
	return w.session.Snapshot()
}

func (w *recordingTunnelCreateWorkflow) RecordTunnel(ctx context.Context, tunnelID, operationID string) error {
	if w.harness.event("workflow.tunnel") {
		return ctx.Err()
	}
	return w.session.RecordTunnel(ctx, tunnelID, operationID)
}

func (w *recordingTunnelCreateWorkflow) RecordDomain(ctx context.Context, index int, domainID string) error {
	if w.harness.event("workflow.domain") {
		return ctx.Err()
	}
	return w.session.RecordDomain(ctx, index, domainID)
}

func (w *recordingTunnelCreateWorkflow) Complete(ctx context.Context) error {
	if w.harness.event("workflow.complete") {
		return ctx.Err()
	}
	return w.session.Complete(ctx)
}

func (w *recordingTunnelCreateWorkflow) Close() error {
	w.harness.events.add("workflow.close")
	return w.session.Close()
}

type workflowAcceptanceWriter struct {
	harness *workflowAcceptanceHarness
	buffer  bytes.Buffer
	wrote   bool
}

type workflowAcceptanceStderr struct {
	harness *workflowAcceptanceHarness
	buffer  *bytes.Buffer
}

func (w *workflowAcceptanceStderr) Write(body []byte) (int, error) {
	if w.harness.trackWait {
		w.harness.events.add("stderr.progress")
	}
	return w.buffer.Write(body)
}

func (w *workflowAcceptanceWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		if w.harness.event("output") {
			return 0, w.harness.commandContext.Err()
		}
	}
	return w.buffer.Write(body)
}

type workflowAcceptanceHarness struct {
	t                    *testing.T
	events               workflowAcceptanceEvents
	stateRoot            string
	server               *httptest.Server
	ready                bool
	selfhost             bool
	dnsVerified          bool
	trackWait            bool
	keySequence          int
	createCalls          int
	completedReplay      bool
	failedReplay         bool
	wrongReplayOperation bool
	wrongReplayTunnel    bool
	domainCalls          int
	cancelAt             string
	cancel               context.CancelFunc
	commandContext       context.Context
	lastRequest          tunnelCreateWorkflowRequest
	hasRequest           bool
	interruptFirstCreate bool
}

func newWorkflowAcceptanceHarness(t *testing.T) *workflowAcceptanceHarness {
	t.Helper()
	h := &workflowAcceptanceHarness{t: t, stateRoot: t.TempDir()}
	h.server = httptest.NewServer(http.HandlerFunc(h.serveControlPlane))
	t.Cleanup(h.server.Close)

	oldClient := tunnelClientForCommand
	oldWorkflow := beginTunnelCreateWorkflowForCommand
	oldKey := newTunnelIdempotencyKey
	oldProbe, oldRepair := tunnelHostRuntimeProbe, tunnelHostRuntimeRepair
	t.Cleanup(func() {
		tunnelClientForCommand = oldClient
		beginTunnelCreateWorkflowForCommand = oldWorkflow
		newTunnelIdempotencyKey = oldKey
		tunnelHostRuntimeProbe, tunnelHostRuntimeRepair = oldProbe, oldRepair
	})

	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", h.stateRoot)
	tunnelHostRuntimeProbe = func(ctx context.Context, _ string) error {
		if !h.ready {
			h.events.add("hostd.probe.unready")
			return errors.New("hostd is stopped")
		}
		if h.event("hostd.readiness") {
			return ctx.Err()
		}
		return nil
	}
	tunnelHostRuntimeRepair = func(ctx context.Context) error {
		if h.event("service.install") {
			return ctx.Err()
		}
		if h.event("service.start") {
			return ctx.Err()
		}
		h.ready = true
		return nil
	}
	tunnelClientForCommand = func(command *cobra.Command) (*api.Client, error) {
		if err := ensureTunnelHostRuntime(command.Context(), h.stateRoot); err != nil {
			return nil, err
		}
		return api.New(h.server.URL, config.Credential{AccessToken: "client-session"}, h.server.Client()), nil
	}
	newTunnelIdempotencyKey = func() (string, error) {
		h.keySequence++
		return fmt.Sprintf("operation_00000000-0000-4000-8000-%012d", h.keySequence), nil
	}
	beginTunnelCreateWorkflowForCommand = func(ctx context.Context, request tunnelCreateWorkflowRequest) (tunnelCreateWorkflow, error) {
		h.events.add("workflow.begin")
		h.lastRequest, h.hasRequest = request, true
		nameDigest := sha256.Sum256([]byte(request.Name))
		session, err := tunnelcreatejournal.Begin(ctx, tunnelcreatejournal.Config{
			StateRoot: h.stateRoot, HostID: "host_workflow", NameDigest: hex.EncodeToString(nameDigest[:]),
			RequestDigest: request.RequestDigest, DomainCount: request.DomainCount, ExpiresAt: request.ExpiresAt,
			NewKey: tunnelKey,
		})
		if err != nil {
			return nil, err
		}
		return &recordingTunnelCreateWorkflow{harness: h, session: session}, nil
	}
	return h
}

func (h *workflowAcceptanceHarness) event(name string) bool {
	h.events.add(name)
	if h.cancelAt == name && h.cancel != nil {
		h.cancel()
		return true
	}
	return false
}

func (h *workflowAcceptanceHarness) serveControlPlane(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels":
		if h.event("control.tunnel.create") {
			w.WriteHeader(499)
			return
		}
		h.createCalls++
		if key := r.Header.Get("Idempotency-Key"); key != "operation_00000000-0000-4000-8000-000000000001" {
			h.t.Errorf("tunnel idempotency key=%q", key)
		}
		var input api.TunnelCreateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			h.t.Errorf("decode tunnel input: %v", err)
			return
		}
		if input.Name != "workflow" || input.AccessMode != "public" || input.Origin != (api.TunnelOriginInput{Scheme: "http", Address: "127.0.0.1:8080"}) {
			h.t.Errorf("tunnel input=%#v", input)
		}
		tunnel := validCommandTunnel()
		tunnel.ID, tunnel.Name, tunnel.AccessMode = "tun_workflow", input.Name, input.AccessMode
		tunnel.ETag = `"tunnel:tun_workflow:1"`
		if h.selfhost {
			tunnel.StableEndpoint, tunnel.SummaryCode = "", "custom_domain_required"
		}
		operation := validCommandOperation("tunnel", tunnel.ID)
		operation.ID = "operation_workflow"
		if h.createCalls > 1 && h.wrongReplayOperation {
			operation.ID = "operation_wrong"
		}
		if h.createCalls > 1 && h.wrongReplayTunnel {
			operation.ResourceID = "tun_wrong"
		}
		if h.createCalls > 1 && h.completedReplay {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
		} else {
			operation.State, operation.Phase = "running", "connecting"
			if h.interruptFirstCreate && h.createCalls == 1 {
				operation.State, operation.Phase = "failed", "failed" // interrupts the first attempt after the journal records the tunnel
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
		}
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/tunnels/tun_workflow" || r.URL.Path == "/v1/tunnels/tun_wrong"):
		tunnel := validCommandTunnel()
		tunnel.ID, tunnel.Name, tunnel.AccessMode = strings.TrimPrefix(r.URL.Path, "/v1/tunnels/"), "workflow", "public"
		tunnel.ETag = `"tunnel:` + tunnel.ID + `:1"`
		if h.selfhost {
			if h.dnsVerified {
				tunnel.StableEndpoint, tunnel.SummaryCode = "https://app.example.test", "ready"
			} else {
				tunnel.StableEndpoint, tunnel.SummaryCode = "", "custom_domain_required"
			}
		}
		w.Header().Set("ETag", tunnel.ETag)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/operation_workflow":
		if h.trackWait {
			h.events.add("control.operation.tunnel")
		}
		operation := validCommandOperation("tunnel", "tun_workflow")
		operation.ID = "operation_workflow"
		if h.failedReplay {
			operation.State, operation.Phase = "failed", "failed"
		} else if h.trackWait {
			operation.State, operation.Phase, operation.Progress = "succeeded", "ready", 100
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/operation_domain_workflow":
		if !h.trackWait {
			h.t.Errorf("unexpected domain operation poll")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.events.add("control.operation.domain")
		operation := validCommandOperation("domain_binding", "domain_workflow")
		operation.ID = "operation_domain_workflow"
		operation.State, operation.Phase, operation.Progress = "succeeded", "ready", 100
		_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_workflow/routes":
		if h.event("control.routes.list") {
			w.WriteHeader(499)
			return
		}
		route := validCommandRoute("route_workflow", "default")
		route.TunnelID = "tun_workflow"
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels/tun_workflow/domains":
		if h.event("control.domain.create") {
			w.WriteHeader(499)
			return
		}
		h.domainCalls++
		if key := r.Header.Get("Idempotency-Key"); key != "operation_00000000-0000-4000-8000-000000000002" {
			h.t.Errorf("domain idempotency key=%q", key)
		}
		var input api.TunnelDomainInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Hostname != "app.example.test" || input.RouteID != "route_workflow" {
			h.t.Errorf("domain input=%#v error=%v", input, err)
		}
		domain := validCommandDomain("domain_workflow", "app.example.test")
		domain.TunnelID, domain.RouteID = "tun_workflow", "route_workflow"
		if h.selfhost && !h.dnsVerified {
			domain.State = "waiting_dns"
		}
		operation := validCommandOperation("domain_binding", domain.ID)
		operation.ID = "operation_domain_workflow"
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainMutation{Domain: domain, Operation: operation, Replayed: h.domainCalls > 1, Changed: h.domainCalls == 1}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_workflow/domains/domain_workflow":
		domain := validCommandDomain("domain_workflow", "app.example.test")
		domain.TunnelID, domain.RouteID = "tun_workflow", "route_workflow"
		if h.selfhost && !h.dnsVerified {
			domain.State = "waiting_dns"
		}
		w.Header().Set("ETag", domain.ETag)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": domain})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_workflow/domains/domain_workflow/instructions":
		if h.event("control.domain.instructions") {
			w.WriteHeader(499)
			return
		}
		instructions := api.TunnelDNSInstructions{
			Schema: api.TunnelV1Schema, Kind: "dns_instructions", TunnelID: "tun_workflow", DomainID: "domain_workflow",
			Hostname: "app.example.test", Provider: "generic", CertificateStrategy: "managed", VerificationState: "waiting_dns",
			Records: []api.TunnelDNSRecord{{Name: "app.example.test", Type: "CNAME", Value: "edge.example.test", TTL: 300}}, Note: "Add this authoritative record.",
		}
		if h.selfhost && h.dnsVerified {
			instructions.VerificationState = "verified"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": instructions})
	default:
		h.t.Errorf("unexpected request=%s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *workflowAcceptanceHarness) execute(jsonOutput, withDomain bool) (string, string, error) {
	return h.executeWithWait(jsonOutput, withDomain, false)
}

func (h *workflowAcceptanceHarness) executeWithWait(jsonOutput, withDomain, wait bool) (string, string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	h.commandContext, h.cancel = ctx, cancel
	defer cancel()
	out := &workflowAcceptanceWriter{harness: h}
	var stderr bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(out)
	if h.trackWait {
		command.SetErr(&workflowAcceptanceStderr{harness: h, buffer: &stderr})
	} else {
		command.SetErr(&stderr)
	}
	args := []string{"create", "workflow", "--port", "8080"}
	if withDomain {
		args = append(args, "--domain", "app.example.test")
	}
	if jsonOutput {
		args = append(args, "--json")
	}
	if wait {
		args = append(args, "--wait", "--timeout", "1s")
	}
	command.SetArgs(args)
	err := command.Execute()
	return out.buffer.String(), stderr.String(), err
}

func (h *workflowAcceptanceHarness) assertJournalLockReleased(t *testing.T) {
	t.Helper()
	if !h.hasRequest {
		return
	}
	nameDigest := sha256.Sum256([]byte(h.lastRequest.Name))
	session, err := tunnelcreatejournal.Begin(t.Context(), tunnelcreatejournal.Config{
		StateRoot: h.stateRoot, HostID: "host_workflow", NameDigest: hex.EncodeToString(nameDigest[:]),
		RequestDigest: h.lastRequest.RequestDigest, DomainCount: h.lastRequest.DomainCount, ExpiresAt: h.lastRequest.ExpiresAt,
		NewKey: tunnelKey,
	})
	if err != nil {
		t.Fatalf("workflow lock was not released: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertWorkflowAcceptanceSecretSafe(t *testing.T, values ...string) {
	t.Helper()
	for _, value := range values {
		for _, secret := range []string{workflowAcceptanceSecret, "operation_00000000-0000-4000-8000-000000000001", "operation_00000000-0000-4000-8000-000000000002", "operation_00000000-0000-4000-8000-000000000003"} {
			if strings.Contains(value, secret) {
				t.Fatalf("secret %q escaped in %q", secret, value)
			}
		}
	}
}

func TestTunnelCreateWorkflowOrdersServiceControlDomainAndOutput(t *testing.T) {
	h := newWorkflowAcceptanceHarness(t)
	stdout, stderr, err := h.execute(true, true)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"hostd.probe.unready", "service.install", "service.start", "hostd.readiness", "workflow.begin",
		"control.tunnel.create", "workflow.tunnel", "control.routes.list", "control.domain.create", "workflow.domain",
		"control.domain.instructions", "output", "workflow.complete", "workflow.close",
	}
	if got := h.events.snapshot(); strings.Join(got, "|") != strings.Join(wantOrder, "|") {
		t.Fatalf("order=\n%s\nwant=\n%s", strings.Join(got, "\n"), strings.Join(wantOrder, "\n"))
	}
	if stderr != "" || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, `"kind":"tunnel_create"`) || !strings.Contains(stdout, `"hostname":"app.example.test"`) || !strings.Contains(stdout, `"edge.example.test"`) {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	assertWorkflowAcceptanceSecretSafe(t, stdout, stderr)
	h.assertJournalLockReleased(t)
}

func TestTunnelCreateWorkflowHumanOutputIsFinalAndSecretSafe(t *testing.T) {
	h := newWorkflowAcceptanceHarness(t)
	stdout, stderr, err := h.execute(false, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Created tunnel workflow (tun_workflow)", "https://123e4567-e89b-42d3-a456-426614174000.tunnels.example.test -> http://127.0.0.1:8080", "DNS for app.example.test:", "app.example.test\tCNAME\tedge.example.test\t300"} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("output missing %q: %s", expected, stdout)
		}
	}
	if stderr != "Creating tunnel… Ctrl+C to cancel.\n" {
		t.Fatalf("stderr=%q", stderr)
	}
	assertWorkflowAcceptanceSecretSafe(t, stdout, stderr)
}

func TestTunnelCreateSelfhostShowsDNSBeforeReadinessAndResumesAfterVerification(t *testing.T) {
	h := newWorkflowAcceptanceHarness(t)
	h.selfhost = true
	firstOut, firstErrOut, err := h.execute(true, true)
	if err != nil || firstErrOut != "" {
		t.Fatalf("pending create stdout=%q stderr=%q error=%v", firstOut, firstErrOut, err)
	}
	var pending tunnelCreateOutput
	if err := json.Unmarshal([]byte(firstOut), &pending); err != nil {
		t.Fatal(err)
	}
	if !pending.Pending || pending.Tunnel.StableEndpoint != "" || pending.Tunnel.SummaryCode != "custom_domain_required" || len(pending.Domains) != 1 || pending.Domains[0].Instructions.VerificationState != "waiting_dns" || !strings.Contains(pending.Recovery, "pb tunnel domain verify tun_workflow app.example.test") {
		t.Fatalf("pending result=%+v", pending)
	}
	if len(pending.Domains[0].Instructions.Records) != 1 || pending.Domains[0].Instructions.Records[0] != (api.TunnelDNSRecord{Name: "app.example.test", Type: "CNAME", Value: "edge.example.test", TTL: 300}) {
		t.Fatalf("pending DNS instructions=%+v", pending.Domains[0].Instructions.Records)
	}
	firstEvents := h.events.snapshot()
	if !strings.Contains(strings.Join(firstEvents, "|"), "control.domain.instructions|output|workflow.close") {
		t.Fatalf("pending ordering=%v", firstEvents)
	}
	nameDigest := sha256.Sum256([]byte(h.lastRequest.Name))
	journal, err := tunnelcreatejournal.Begin(t.Context(), tunnelcreatejournal.Config{
		StateRoot: h.stateRoot, HostID: "host_workflow", NameDigest: hex.EncodeToString(nameDigest[:]),
		RequestDigest: h.lastRequest.RequestDigest, DomainCount: h.lastRequest.DomainCount, ExpiresAt: h.lastRequest.ExpiresAt,
		NewKey: tunnelKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstJournal := journal.Snapshot()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if firstJournal.Stage != tunnelcreatejournal.StageDomainsReady || firstJournal.TunnelID != "tun_workflow" || firstJournal.Domains[0].ID != "domain_workflow" {
		t.Fatalf("pending journal=%+v", firstJournal)
	}

	h.dnsVerified = true
	secondOut, secondErrOut, err := h.execute(true, true)
	if err != nil || secondErrOut != "" {
		t.Fatalf("resumed create stdout=%q stderr=%q error=%v", secondOut, secondErrOut, err)
	}
	var ready tunnelCreateOutput
	if err := json.Unmarshal([]byte(secondOut), &ready); err != nil {
		t.Fatal(err)
	}
	if ready.Pending || ready.Tunnel.StableEndpoint != "https://app.example.test" || !ready.Domains[0].Replayed {
		t.Fatalf("resumed result=%+v", ready)
	}
	if events := h.events.snapshot(); h.createCalls != 2 || h.domainCalls != 1 {
		t.Fatalf("resume events=%v creates=%d domains=%d", events, h.createCalls, h.domainCalls)
	}
	h.assertJournalLockReleased(t)
}

func TestTunnelCreateWaitShowsDNSThenWaitsForTunnelThenDomainOperation(t *testing.T) {
	h := newWorkflowAcceptanceHarness(t)
	h.trackWait = true
	stdout, stderr, err := h.executeWithWait(true, true, true)
	if err != nil {
		t.Fatalf("create with --wait: stdout=%q stderr=%q error=%v", stdout, stderr, err)
	}
	assertWorkflowAcceptanceSecretSafe(t, stdout, stderr)
	if !strings.Contains(stderr, "app.example.test\tCNAME\tedge.example.test\t300") {
		t.Fatalf("DNS records were not shown before waiting: %q", stderr)
	}
	events := h.events.snapshot()
	dnsProgress, tunnelWait, domainWait := -1, -1, -1
	for index, event := range events {
		switch event {
		case "stderr.progress":
			if dnsProgress == -1 {
				dnsProgress = index
			}
		case "control.operation.tunnel":
			tunnelWait = index
		case "control.operation.domain":
			domainWait = index
		}
	}
	if dnsProgress < 0 || tunnelWait <= dnsProgress || domainWait <= tunnelWait {
		t.Fatalf("readiness wait order=%v", events)
	}
	var result tunnelCreateOutput
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Pending || len(result.Domains) != 1 || result.Domains[0].Operation.State != "succeeded" || result.Domains[0].Instructions.VerificationState != "waiting_dns" {
		t.Fatalf("create result=%+v", result)
	}
}

func TestTunnelCreateWorkflowResumesExactIdempotentRequestAfterInterruptedOperation(t *testing.T) {
	h := newWorkflowAcceptanceHarness(t)
	h.interruptFirstCreate = true
	firstOut, firstErrOut, err := h.execute(true, false)
	var outcome *TunnelOperationOutcomeError
	if !errors.As(err, &outcome) {
		t.Fatalf("first error=%T %v", err, err)
	}
	if firstOut != "" || firstErrOut != "" {
		t.Fatalf("first stdout=%q stderr=%q", firstOut, firstErrOut)
	}
	assertWorkflowAcceptanceSecretSafe(t, firstOut, firstErrOut, err.Error())

	secondOut, secondErrOut, err := h.execute(true, false)
	if err != nil {
		t.Fatal(err)
	}
	if h.createCalls != 2 || !strings.Contains(secondOut, `"replayed":true`) || secondErrOut != "" {
		t.Fatalf("create calls=%d stdout=%q stderr=%q", h.createCalls, secondOut, secondErrOut)
	}
	assertWorkflowAcceptanceSecretSafe(t, secondOut, secondErrOut)
	events := strings.Join(h.events.snapshot(), "|")
	if strings.Count(events, "control.tunnel.create") != 2 || strings.Count(events, "workflow.tunnel") != 2 || strings.Count(events, "workflow.complete") != 1 {
		t.Fatalf("resume events=%s", events)
	}
	h.assertJournalLockReleased(t)
}

func TestTunnelCreateWorkflowCancellationStopsAtEveryBoundaryAndReleasesOwnership(t *testing.T) {
	boundaries := []string{
		"service.start", "hostd.readiness", "control.tunnel.create", "workflow.tunnel",
		"control.routes.list", "control.domain.create", "workflow.domain", "control.domain.instructions",
		"output", "workflow.complete",
	}
	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			h := newWorkflowAcceptanceHarness(t)
			h.cancelAt = boundary
			stdout, stderr, err := h.execute(true, true)
			if err == nil || !errors.Is(h.commandContext.Err(), context.Canceled) {
				t.Fatalf("boundary=%s error=%T %v context=%v", boundary, err, err, h.commandContext.Err())
			}
			if strings.Contains(strings.Join(h.events.snapshot(), "|"), "workflow.complete|workflow.close") && boundary != "workflow.complete" {
				t.Fatalf("completed after cancellation: %v", h.events.snapshot())
			}
			assertWorkflowAcceptanceSecretSafe(t, stdout, stderr, err.Error())
			h.assertJournalLockReleased(t)
		})
	}
}

func TestTunnelCreateWorkflowHarnessRejectsTrailingOutputSecrets(t *testing.T) {
	h := newWorkflowAcceptanceHarness(t)
	stdout, stderr, err := h.execute(true, false)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkflowAcceptanceSecretSafe(t, stdout, stderr)
	var output tunnelCreateOutput
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		t.Fatalf("unsafe output=%q decode=%v", stdout, err)
	}
}

func TestTunnelCreateWorkflowCanonicalReplayIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                                           string
		completed, wrongOperation, wrongTunnel, failed bool
	}{
		{name: "completed", completed: true}, {name: "failed", completed: true, failed: true}, {name: "wrong_operation", wrongOperation: true}, {name: "wrong_tunnel", wrongTunnel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWorkflowAcceptanceHarness(t)
			h.interruptFirstCreate = true
			_, _, err := h.execute(true, false)
			var interrupted *TunnelOperationOutcomeError
			if !errors.As(err, &interrupted) {
				t.Fatalf("first create: %v", err)
			}
			h.completedReplay, h.wrongReplayOperation, h.wrongReplayTunnel = tc.completed, tc.wrongOperation, tc.wrongTunnel
			h.failedReplay = tc.failed
			out, _, err := h.execute(true, false)
			if tc.failed {
				var outcome *TunnelOperationOutcomeError
				if !errors.As(err, &outcome) || out != "" {
					t.Fatalf("failed replay: %s %v", out, err)
				}
			} else if tc.completed {
				if err != nil || !strings.Contains(out, `"replayed":true`) {
					t.Fatalf("completed replay: %s %v", out, err)
				}
			} else if !errors.Is(err, api.ErrUnsafeTunnelResponse) {
				t.Fatalf("identity mismatch accepted: %v", err)
			}
		})
	}
}
