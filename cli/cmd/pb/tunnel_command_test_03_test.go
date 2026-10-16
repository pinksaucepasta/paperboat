package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelcreatejournal"
	"github.com/spf13/cobra"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTunnelRouteSelectorsResolveNamesToCanonicalIDs(t *testing.T) {
	route := api.TunnelRoute{Schema: api.TunnelV1Schema, Kind: "route", ID: "route_1", TunnelID: "tun_1", Name: "default", Protocol: "http", HostMatch: api.TunnelRouteHostMatch{Type: "catch_all"}, Origin: api.TunnelRouteOrigin{Scheme: "http", Address: "127.0.0.1:8080", PreserveHost: true}, Priority: 0, ConnectTimeoutMS: 10000, IdleTimeoutMS: 300000, MaxConcurrentStreams: 128, DesiredState: "active", Generation: 1, ETag: `"route:route_1:1"`}

	t.Run("domain add", func(t *testing.T) {
		withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/routes":
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
			case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels/tun_1/domains":
				var input api.TunnelDomainInput
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				if input.RouteID != route.ID {
					t.Fatalf("route id=%q", input.RouteID)
				}
				domain := api.TunnelDomain{Schema: api.TunnelV1Schema, Kind: "domain_binding", ID: "domain_1", AccountID: "account_1", TunnelID: "tun_1", RouteID: route.ID, Hostname: "app.example.test", MatchType: "exact", State: "waiting_dns", DNS: api.TunnelDomainDNS{Target: "edge.example.test"}, Certificate: api.TunnelDomainCertificate{State: "not_requested"}, Generation: 1, ETag: `"domain:domain_1:1"`}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainMutation{Domain: domain, Operation: validCommandOperation("domain_binding", domain.ID), Changed: true}})
			default:
				t.Fatalf("request=%s %s", r.Method, r.URL.Path)
			}
		})
		command := tunnelCobraCommandV1()
		command.SetArgs([]string{"domain", "add", "tun_1", "app.example.test", "--route", "default"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("route update", func(t *testing.T) {
		updated := route
		withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/routes":
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
			case r.Method == http.MethodPatch && r.URL.Path == "/v1/tunnels/tun_1/routes/route_1":
				if r.Header.Get("If-Match") != route.ETag {
					t.Fatalf("if-match=%q", r.Header.Get("If-Match"))
				}
				var patch map[string]any
				if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
					t.Fatal(err)
				}
				if len(patch) != 2 || patch["name"] != "renamed" || patch["path_prefix"] != "/renamed" {
					t.Fatalf("route patch did not preserve requested fields: %#v", patch)
				}
				updated.Name = patch["name"].(string)
				path := patch["path_prefix"].(string)
				updated.PathPrefix = &path
				updated.Generation = 2
				updated.ETag = `"route:route_1:2"`
				operation := validCommandOperation("route", route.ID)
				operation.State = "running"
				_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
			case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/routes/route_1":
				w.Header().Set("ETag", updated.ETag)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": updated})
			default:
				t.Fatalf("request=%s %s", r.Method, r.URL.Path)
			}
		})
		command := newRootCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetArgs([]string{"tunnel", "route", "update", "tun_1", "default", "--name", "renamed", "--path", "/renamed", "--json"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		var result api.TunnelRouteMutation
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Route.Name != "renamed" || result.Route.PathPrefix == nil || *result.Route.PathPrefix != "/renamed" {
			t.Fatalf("completed route fields missing: %#v", result.Route)
		}
	})
	t.Run("route remove", func(t *testing.T) {
		t.Setenv(config.EnvConfigPath, filepath.Join(t.TempDir(), "config.json"))
		withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/routes":
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
			case r.Method == http.MethodDelete && r.URL.Path == "/v1/tunnels/tun_1/routes/route_1":
				if r.Header.Get("If-Match") != route.ETag {
					t.Fatalf("if-match=%q", r.Header.Get("If-Match"))
				}
				removed := route
				removed.DesiredState = "deleted"
				removed.Generation = 2
				removed.ETag = `"route:route_1:2"`
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: removed, Operation: validCommandOperation("route", route.ID), Changed: true}})
			default:
				t.Fatalf("request=%s %s", r.Method, r.URL.Path)
			}
		})
		command := tunnelCobraCommandV1()
		var preview bytes.Buffer
		command.SetOut(&preview)
		command.SetArgs([]string{"route", "remove", "tun_1", "default"})
		if err := command.Execute(); err == nil || err.(exitCodeError).code != 2 {
			t.Fatalf("preview error=%v", err)
		}
		command = tunnelCobraCommandV1()
		command.SetArgs([]string{"route", "remove", "tun_1", "default", "--confirm", previewConfirmationCode(t, preview.String())})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTunnelDomainInstructionsResolveHostnameAndRenderAuthoritativeRecords(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		domain := api.TunnelDomain{Schema: api.TunnelV1Schema, Kind: "domain_binding", ID: "domain_1", AccountID: "account_1", TunnelID: "tun_1", RouteID: "route_1", Hostname: "app.example.test", MatchType: "exact", State: "waiting_dns", DNS: api.TunnelDomainDNS{Target: "edge.example.test"}, Certificate: api.TunnelDomainCertificate{State: "not_requested"}, Generation: 1, ETag: `"domain:domain_1:1"`}
		switch r.URL.Path {
		case "/v1/tunnels/tun_1/domains":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainPage{Items: []api.TunnelDomain{domain}}})
		case "/v1/tunnels/tun_1/domains/domain_1/instructions":
			instructions := api.TunnelDNSInstructions{Schema: api.TunnelV1Schema, Kind: "dns_instructions", TunnelID: "tun_1", DomainID: "domain_1", Hostname: domain.Hostname, Provider: "generic", Records: []api.TunnelDNSRecord{{Name: domain.Hostname, Type: "CNAME", Value: "edge.example.test", TTL: 300}}, CertificateStrategy: "managed", VerificationState: "waiting_dns", Note: "Add this record."}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": instructions})
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"domain", "instructions", "tun_1", "app.example.test"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "app.example.test\tCNAME\tedge.example.test\t300\nAdd this record.\n" {
		t.Fatalf("output=%q", output.String())
	}
}

func TestTunnelDomainInstructionsRenderPublicTCPPortSeparately(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		domain := api.TunnelDomain{Schema: api.TunnelV1Schema, Kind: "domain_binding", ID: "domain_tcp", AccountID: "account_1", TunnelID: "tun_1", RouteID: "route_tcp", Hostname: "db.example.test", MatchType: "exact", CertificateStrategy: "none", State: "verified", DNS: api.TunnelDomainDNS{Target: "edge.example.test"}, Certificate: api.TunnelDomainCertificate{State: "not_applicable"}, Generation: 1, ETag: `"domain:domain_tcp:1"`}
		switch r.URL.Path {
		case "/v1/tunnels/tun_1/domains":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainPage{Items: []api.TunnelDomain{domain}}})
		case "/v1/tunnels/tun_1/domains/domain_tcp/instructions":
			instructions := api.TunnelDNSInstructions{Schema: api.TunnelV1Schema, Kind: "dns_instructions", TunnelID: "tun_1", DomainID: domain.ID, Hostname: domain.Hostname, Provider: "generic", Records: []api.TunnelDNSRecord{{Name: domain.Hostname, Type: "CNAME", Value: "edge.example.test", TTL: 300}}, CertificateStrategy: "none", VerificationState: "verified", PublicTCPPort: 25432, Note: "Application TLS is passed through unchanged."}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": instructions})
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"domain", "instructions", "tun_1", "db.example.test"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "db.example.test\tCNAME\tedge.example.test\t300\nTCP endpoint\tdb.example.test:25432\nApplication TLS is passed through unchanged.\n" {
		t.Fatalf("output=%q", output.String())
	}
}

func TestTunnelRouteMutationRetriesTransientFailureWithSameIdempotencyKey(t *testing.T) {
	oldDelay := tunnelRequestRetryDelay
	tunnelRequestRetryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { tunnelRequestRetryDelay = oldDelay })
	var calls int
	var keys []string
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "temporarily_unavailable", "message": "control plane is retrying"}})
			return
		}
		route := validCommandRoute("route_1", "api")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: route, Operation: validCommandOperation("route", route.ID), Changed: true}})
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "add", "tun_1", "--name", "api", "--to", "http://127.0.0.1:8080"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("calls=%d keys=%v", calls, keys)
	}
}

func TestTunnelRouteMutationReportsTerminalFailureWithoutWait(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		route := validCommandRoute("route_1", "api")
		operation := validCommandOperation("route", route.ID)
		operation.Phase = "failed"
		operation.State = "failed"
		operation.Progress = 100
		operation.Error = &api.PreviewTunnelAPIError{Schema: api.TunnelV1Schema, Kind: "error", Code: "origin_unreachable", Component: "edge", Message: "origin unavailable", Outcome: "failed", RequestID: "request_1", CorrelationID: operation.CorrelationID}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: route, Operation: operation, Changed: true}})
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "add", "tun_1", "--name", "api", "--to", "http://127.0.0.1:8080"})
	err := command.Execute()
	var outcome *TunnelOperationOutcomeError
	if !errors.As(err, &outcome) || outcome.Operation.State != "failed" || outcome.Operation.ID != "operation_1" {
		t.Fatalf("error=%#v outcome=%#v", err, outcome)
	}
}

func TestTunnelRouteAddRejectsHTTPOverTCPOrigin(t *testing.T) {
	requests := 0
	withTunnelCommandClient(t, func(http.ResponseWriter, *http.Request) { requests++ })
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "add", "tun_1", "--name", "api", "--to", "tcp://127.0.0.1:8080"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "HTTP routes cannot target a tcp origin") {
		t.Fatalf("error=%v", err)
	}
	if requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}

type readyConnectorRecoveryWorkflow struct {
	journal   tunnelcreatejournal.Journal
	completed bool
}

func (w *readyConnectorRecoveryWorkflow) Snapshot() tunnelcreatejournal.Journal {
	return w.journal
}

func (w *readyConnectorRecoveryWorkflow) RecordTunnel(_ context.Context, tunnelID, operationID string) error {
	if tunnelID != w.journal.TunnelID || operationID != w.journal.OperationID {
		return tunnelcreatejournal.ErrRequestMismatch
	}
	return nil
}

func (w *readyConnectorRecoveryWorkflow) RecordConnectorReady(context.Context) error {
	return tunnelcreatejournal.ErrInvalid
}

func (w *readyConnectorRecoveryWorkflow) RecordDomain(_ context.Context, _ int, _ string) error {
	return tunnelcreatejournal.ErrInvalid
}

func (w *readyConnectorRecoveryWorkflow) Complete(context.Context) error {
	w.completed = true
	return nil
}

func (*readyConnectorRecoveryWorkflow) Close() error { return nil }

func TestTunnelCreateReadyConnectorJournalRecoversWithoutReenrollment(t *testing.T) {
	var requests []string
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels":
			value := validCommandTunnel()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: value, Operation: validCommandOperation("tunnel", value.ID), Replayed: true}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/connectors":
			readyAt := time.Unix(4, 0).UTC()
			connector := api.TunnelConnector{
				Schema: api.TunnelV1Schema, Kind: "connector", ID: "connector_1", TunnelID: "tun_1", HostID: "host_1",
				CredentialReference: "protected-file://paperboat/connector_1", RotationGeneration: 1, ProtocolVersion: "1.0",
				DesiredState: "active", DrainState: "accepting", Generation: 1, ETag: `"connector:connector_1:1"`, ReadyAt: &readyAt,
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelConnectorPage{Items: []api.TunnelConnector{connector}}})
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	workflow := &readyConnectorRecoveryWorkflow{}
	oldWorkflow := beginTunnelCreateWorkflowForCommand
	beginTunnelCreateWorkflowForCommand = func(_ context.Context, request tunnelCreateWorkflowRequest) (tunnelCreateWorkflow, error) {
		workflow.journal = tunnelcreatejournal.Journal{
			Schema: tunnelcreatejournal.Schema, HostID: "host_1", RequestDigest: request.RequestDigest, TunnelKey: "idem_test",
			TunnelID: "tun_1", OperationID: "operation_1", Stage: tunnelcreatejournal.StageConnectorReady,
			ExpiresAt: request.ExpiresAt, Domains: nil,
		}
		return workflow, nil
	}
	t.Cleanup(func() { beginTunnelCreateWorkflowForCommand = oldWorkflow })
	runtimeCalls := 0
	oldRuntime := tunnelConnectorAddRuntime
	tunnelConnectorAddRuntime = func(*cobra.Command, string) error {
		runtimeCalls++
		return errors.New("unexpected connector re-enrollment")
	}
	t.Cleanup(func() { tunnelConnectorAddRuntime = oldRuntime })
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"create", "demo", "--port", "8080", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if runtimeCalls != 0 || !workflow.completed {
		t.Fatalf("runtime calls=%d workflow completed=%v", runtimeCalls, workflow.completed)
	}
	if strings.Join(requests, ",") != "POST /v1/tunnels,GET /v1/tunnels/tun_1/connectors" {
		t.Fatalf("requests=%v", requests)
	}
	if !strings.Contains(output.String(), `"connector_id":"connector_1"`) || !strings.Contains(output.String(), `"replayed":true`) {
		t.Fatalf("output=%q", output.String())
	}
}

func TestTunnelCreateTeamAudienceUsesCanonicalAPI(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tunnels" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Idempotency-Key") != "idem_test" {
			t.Fatalf("idempotency = %q", r.Header.Get("Idempotency-Key"))
		}
		var body api.TunnelCreateInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AccessMode != "team" || body.Origin.Scheme != "https" || body.Origin.Address != "origin.example:443" {
			t.Fatalf("body = %#v", body)
		}
		value := validCommandTunnel()
		value.Generation = 1
		value.ETag = `"tunnel:tun_1:1"`
		value.Name = body.Name
		value.AccessMode = body.AccessMode
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: value, Operation: validCommandOperation("tunnel", value.ID)}})
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"create", "demo", "--from", "https://origin.example:443", "--team", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id":"tun_1"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestTunnelCreateWaitsForCanonicalDomainAndRejectsChangedBinding(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%v", changed), func(t *testing.T) {
			initial := validCommandDomain("domain_wait", "app.example.test")
			initial.State = "issuing_tls"
			initial.Certificate.State = "issuing"
			calls := 0
			withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/tunnels/tun_1/domains/domain_wait" {
					t.Fatalf("request=%s %s", r.Method, r.URL.Path)
				}
				next := validCommandDomain(initial.ID, initial.Hostname)
				if changed {
					next.RouteID = "other_route"
				}
				w.Header().Set("ETag", next.ETag)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": next})
			})
			command := tunnelCreateCommand()
			command.SetContext(context.Background())
			_ = command.Flags().Set("wait", "true")
			_ = command.Flags().Set("timeout", "1s")
			client, _, err := tunnelClient(command)
			if err != nil {
				t.Fatal(err)
			}
			result, err := waitForCreatedTunnelDomain(command, client, initial)
			if changed {
				if !errors.Is(err, api.ErrUnsafeTunnelResponse) {
					t.Fatalf("changed binding=%v", err)
				}
			} else if err != nil || result.State != "ready" {
				t.Fatalf("readiness state=%s err=%v", result.State, err)
			}
			if calls != 1 {
				t.Fatalf("canonical reads=%d", calls)
			}
		})
	}
}
