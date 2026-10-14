package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/spf13/cobra"
)

func TestPreviewCommandDomainsUsesCanonicalRequestAndSafeProjection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	var createDomains []string
	var requests []string
	var ownerSessionID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()

		var body struct {
			OwnerSessionID string   `json:"owner_session_id"`
			Domains        []string `json:"domains"`
		}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode %s %s: %v", r.Method, r.URL.Path, err)
			}
		}

		var value map[string]any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/previews":
			createDomains = append([]string(nil), body.Domains...)
			ownerSessionID = body.OwnerSessionID
			value = previewDomainCommandLease(now, "connecting", ownerSessionID)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/previews/prv_domains":
			value = previewDomainCommandLease(now, "ready", ownerSessionID)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/previews/prv_domains":
			value = previewDomainCommandLease(now, "stopped", ownerSessionID)
			value["allocation_state"], value["edge_state"] = "released", "down"
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:1"`)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/previews" {
			w.Header().Set("X-Paperboat-Operation-ID", "operation_preview_domains")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
	}))
	defer server.Close()

	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	previousClient := previewClientForCommand
	previousMachine := previewMachineID
	previousCarrier := newPreviewCarrier
	t.Cleanup(func() {
		previewClientForCommand = previousClient
		previewMachineID = previousMachine
		newPreviewCarrier = previousCarrier
	})
	previewClientForCommand = func(*cobra.Command) (*api.Client, error) { return client, nil }
	previewMachineID = func() (string, error) { return "machine_cli", nil }
	carrier := &cliPreviewCarrier{ready: make(chan struct{})}
	newPreviewCarrier = func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error) {
		return carrier, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := previewCobraCommandV1()
	var output lockedPreviewWriter
	command.SetOut(&output)
	var errorOutput lockedPreviewWriter
	command.SetErr(&errorOutput)
	command.SetArgs([]string{"3000", "--domain", "BÜCHER.Example.", "--domain", "APP.Example.com.", "--json"})
	result := make(chan error, 1)
	go func() { result <- command.ExecuteContext(ctx) }()

	select {
	case <-carrier.ready:
	case <-time.After(2 * time.Second):
		select {
		case err := <-result:
			t.Fatalf("carrier did not report readiness; command error: %v", err)
		default:
			t.Fatal("carrier did not report readiness")
		}
	}
	deadline := time.After(2 * time.Second)
	for bytes.Count(output.Bytes(), []byte("\n")) < 3 {
		select {
		case <-deadline:
			t.Fatal("preview command did not publish JSON")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	var lease api.PreviewLease
	if err := json.NewDecoder(bytes.NewReader(output.Bytes())).Decode(&lease); err != nil {
		t.Fatalf("JSON output = %q: %v", output.String(), err)
	}
	if lease.ID != "prv_domains" || lease.State != "ready" || lease.Endpoint == "" || len(lease.Domains) != 2 {
		t.Fatalf("lease projection = %#v", lease)
	}
	if got := []string{lease.Domains[0].Hostname, lease.Domains[1].Hostname}; !reflect.DeepEqual(got, []string{"app.example.com", "xn--bcher-kva.example"}) {
		t.Fatalf("domain projection = %#v", got)
	}
	if lease.Domains[0].TargetKind != "preview_lease" || lease.Domains[0].PreviewID != lease.ID || lease.Domains[0].Instructions == nil {
		t.Fatalf("domain binding = %#v", lease.Domains[0])
	}
	if bytes.Contains(output.Bytes(), []byte("token")) || bytes.Contains(output.Bytes(), []byte("private_key")) {
		t.Fatalf("JSON output contains a secret-bearing field: %s", output.String())
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preview command did not stop")
	}
	if errorOutput.Len() != 0 {
		t.Fatalf("JSON emitted prose on stderr: %q", errorOutput.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	if err := decoder.Decode(&lease); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var event struct {
			OK   bool `json:"ok"`
			Data struct {
				Event     string                   `json:"event"`
				PreviewID string                   `json:"preview_id"`
				Domain    api.PreviewDomainSummary `json:"domain"`
			} `json:"data"`
		}
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if !event.OK || event.Data.Event != "domain_status" || event.Data.PreviewID != lease.ID || event.Data.Domain.State != "ready" {
			t.Fatalf("invalid domain event: %#v", event)
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("unexpected trailing output: %v", err)
	}
	mu.Lock()
	gotRequests := append([]string(nil), requests...)
	gotCreateDomains := append([]string(nil), createDomains...)
	mu.Unlock()
	if !reflect.DeepEqual(gotCreateDomains, []string{"app.example.com", "xn--bcher-kva.example"}) {
		t.Fatalf("create domains = %#v", gotCreateDomains)
	}
	if len(gotRequests) < 3 || gotRequests[0] != "POST /v1/previews" || gotRequests[1] != "GET /v1/previews/prv_domains" {
		t.Fatalf("API request sequence = %#v", gotRequests)
	}
}

func previewDomainCommandLease(now time.Time, state, ownerSessionID string) map[string]any {
	lease := map[string]any{
		"schema": api.PreviewTunnelSchemaV1, "kind": "preview_lease", "id": "prv_domains", "account_id": "acct_cli", "actor_id": "actor_cli",
		"owner_machine_id": "machine_cli", "owner_session_id": ownerSessionID, "owner_session_kind": "foreground",
		"target": map[string]string{"scheme": "http", "address": "127.0.0.1:3000"}, "access_mode": "public", "persistent": false,
		"endpoint": "https://preview-domain.preview.example.test", "endpoint_state": "ready", "lease_deadline": now.Add(time.Hour), "user_deadline": nil,
		"state": state, "allocation_state": "pending", "edge_state": "pending", "origin_state": "unknown",
		"created_at": now, "last_renewed_at": now,
	}
	if state == "ready" {
		lease["allocation_state"], lease["edge_state"], lease["origin_state"] = "ready", "ready", "ready"
	}
	lease["domains"] = []map[string]any{
		{
			"id": "domain_app", "target_kind": "preview_lease", "preview_id": "prv_domains", "hostname": "app.example.com", "match_type": "exact", "state": "ready",
			"dns":         map[string]any{"target": "domains.example.test", "observed_records": []string{"domains.example.test"}},
			"certificate": map[string]any{"state": "ready", "reference": "certificate_app", "expires_at": now.Add(30 * 24 * time.Hour)}, "generation": 1, "etag": `"domain_app:1"`,
			"instructions": map[string]any{
				"schema": api.PreviewTunnelSchemaV1, "kind": "dns_instructions", "target_kind": "preview_lease", "preview_id": "prv_domains", "domain_id": "domain_app", "hostname": "app.example.com", "provider": "generic",
				"records": []map[string]any{{"name": "_acme-challenge.app.example.com", "type": "CNAME", "value": "pb-app.challenge.example.test", "ttl": 300}}, "certificate_strategy": "delegated_dns01", "verification_state": "waiting_dns", "note": "Create this CNAME.",
			},
		},
		{
			"id": "domain_bucher", "target_kind": "preview_lease", "preview_id": "prv_domains", "hostname": "xn--bcher-kva.example", "match_type": "exact", "state": "ready",
			"dns": map[string]any{"target": "domains.example.test"}, "certificate": map[string]any{"state": "ready"}, "generation": 1, "etag": `"domain_bucher:1"`,
		},
	}
	return lease
}

func TestPreviewDomainLeaseCreateFetchesAndPrintsAuthoritativeDNSRecords(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/previews":
			value := previewDomainCommandLease(now, "connecting", "session_cli")
			value["endpoint"], value["endpoint_state"] = "", "custom_domain_not_ready"
			domains := value["domains"].([]map[string]any)[:1]
			domain := domains[0]
			domain["state"] = "waiting_dns"
			domain["certificate"].(map[string]any)["state"] = "not_requested"
			delete(domain, "instructions")
			value["domains"] = domains
			w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:1"`)
			w.Header().Set("X-Paperboat-Operation-ID", "operation_preview_domains")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/previews/prv_domains/domains/domain_app/instructions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.PreviewDNSInstructions{
				Schema: api.PreviewTunnelSchemaV1, Kind: "dns_instructions", TargetKind: "preview_lease", PreviewID: "prv_domains", DomainID: "domain_app",
				Hostname: "app.example.com", Provider: "generic",
				Records:             []api.PreviewDNSRecord{{Name: "_acme-challenge.app.example.com", Type: "CNAME", Value: "pb-app.challenge.example.test", TTL: 300}},
				CertificateStrategy: "delegated_dns01", VerificationState: "waiting_dns", Note: "Add this CNAME.",
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	var output bytes.Buffer
	domainClient := &previewDomainLeaseClient{client: client, domains: []string{"app.example.com"}, instructionsWriter: &output}
	_, err := domainClient.Create(context.Background(), preview.LeaseRequest{
		OwnerMachineID: "machine_cli", OwnerSessionID: "session_cli", OwnerSessionKind: "foreground",
		Target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public", IdempotencyKey: "create-domains",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "state=waiting_dns") || !strings.Contains(output.String(), "CNAME _acme-challenge.app.example.com -> pb-app.challenge.example.test (TTL 300)") {
		t.Fatalf("DNS instructions output = %q", output.String())
	}
	if !reflect.DeepEqual(requests, []string{
		"POST /v1/previews",
		"GET /v1/previews/prv_domains/domains/domain_app/instructions",
	}) {
		t.Fatalf("API request order = %#v", requests)
	}
}

func TestPreviewDomainLeaseCreateStopsLeaseWhenDNSInstructionsCannotBeRead(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/previews":
			value := previewDomainCommandLease(now, "connecting", "session_cli")
			value["endpoint"], value["endpoint_state"] = "", "custom_domain_not_ready"
			domains := value["domains"].([]map[string]any)[:1]
			domain := domains[0]
			domain["state"] = "waiting_dns"
			domain["certificate"].(map[string]any)["state"] = "not_requested"
			delete(domain, "instructions")
			value["domains"] = domains
			w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:1"`)
			w.Header().Set("X-Paperboat-Operation-ID", "operation_preview_domains")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/previews/prv_domains/domains/domain_app/instructions":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"dns_unavailable"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/previews/prv_domains":
			value := previewDomainCommandLease(now, "stopped", "session_cli")
			value["endpoint"], value["endpoint_state"] = "", "custom_domain_not_ready"
			value["allocation_state"], value["edge_state"] = "released", "down"
			domains := value["domains"].([]map[string]any)[:1]
			domains[0]["state"] = "waiting_dns"
			domains[0]["certificate"].(map[string]any)["state"] = "not_requested"
			delete(domains[0], "instructions")
			value["domains"] = domains
			w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:2"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	domainClient := &previewDomainLeaseClient{client: client, domains: []string{"app.example.com"}, instructionsWriter: io.Discard}
	_, err := domainClient.Create(context.Background(), preview.LeaseRequest{
		OwnerMachineID: "machine_cli", OwnerSessionID: "session_cli", OwnerSessionKind: "foreground",
		Target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public", IdempotencyKey: "create-domains",
	})
	if err == nil || !strings.Contains(err.Error(), "preview lease was stopped") {
		t.Fatalf("create error = %v, want actionable stopped-lease failure", err)
	}
	if !reflect.DeepEqual(requests, []string{
		"POST /v1/previews",
		"GET /v1/previews/prv_domains/domains/domain_app/instructions",
		"DELETE /v1/previews/prv_domains",
	}) {
		t.Fatalf("API request order = %#v", requests)
	}
}

func TestObservePreviewDomainsDoesNotReportReadyBeforeCertificate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/previews/prv_domains" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		getCalls++
		call := getCalls
		mu.Unlock()
		value := previewDomainCommandLease(now, "ready", "session_cli")
		if call == 1 {
			value = previewDomainCommandLease(now, "connecting", "session_cli")
			domains := value["domains"].([]map[string]any)
			domains[0]["state"] = "waiting_dns"
			domains[0]["certificate"].(map[string]any)["state"] = "issuing"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:1"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
	}))
	defer server.Close()

	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	writer := &previewDomainTestWriter{first: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		observePreviewDomains(context.Background(), client, "prv_domains", []string{"app.example.com"}, writer)
		close(done)
	}()
	select {
	case <-writer.first:
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not publish the initial DNS state")
	}
	initial := writer.String()
	if !strings.Contains(initial, "state=waiting_dns") || strings.Contains(initial, "state=ready") {
		t.Fatalf("observer reported an unready domain as ready: %q", initial)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not stop after readiness")
	}
	final := writer.String()
	if strings.Index(final, "state=waiting_dns") >= strings.Index(final, "state=ready") {
		t.Fatalf("observer status order = %q", final)
	}
}

type previewDomainTestWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	once  sync.Once
	first chan struct{}
}

func (w *previewDomainTestWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(value)
	w.once.Do(func() { close(w.first) })
	return n, err
}

func (w *previewDomainTestWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type previewDomainFailWriter struct {
	writes int
	err    error
}

func (w *previewDomainFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, w.err
	}
	return len(p), nil
}
func TestPreviewDomainJSONOutputFailureStopsSession(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	stopped := false
	server := newPreviewCommandServer(t, func(r *http.Request, body map[string]any) (any, string, int) {
		state, owner := "ready", "session_cli"
		if r.Method == http.MethodPost {
			state = "connecting"
			owner = body["owner_session_id"].(string)
		}
		if r.Method == http.MethodDelete {
			mu.Lock()
			stopped = true
			mu.Unlock()
			state = "stopped"
		}
		value := previewDomainCommandLease(now, state, owner)
		value["domains"] = value["domains"].([]map[string]any)[:1]
		if state == "stopped" {
			value["allocation_state"], value["edge_state"] = "released", "down"
		}
		return value, `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:1"`, http.StatusOK
	})
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	oldClient, oldMachine, oldCarrier := previewClientForCommand, previewMachineID, newPreviewCarrier
	defer func() {
		previewClientForCommand, previewMachineID, newPreviewCarrier = oldClient, oldMachine, oldCarrier
	}()
	previewClientForCommand = func(*cobra.Command) (*api.Client, error) { return client, nil }
	previewMachineID = func() (string, error) { return "machine_cli", nil }
	newPreviewCarrier = func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error) {
		return &cliPreviewCarrier{ready: make(chan struct{})}, nil
	}
	sentinel := errors.New("domain output failed")
	output := &previewDomainFailWriter{err: sentinel}
	command := previewCobraCommandV1()
	command.SetOut(output)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"3000", "--domain", "app.example.com", "--json"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := command.ExecuteContext(ctx)
	if !errors.Is(err, sentinel) {
		t.Fatalf("output failure was lost: %v", err)
	}
	mu.Lock()
	wasStopped := stopped
	mu.Unlock()
	if !wasStopped {
		t.Fatal("output failure left preview active")
	}
}
