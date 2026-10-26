package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelcreatejournal"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveTunnelRouteAndDomainAreAmbiguitySafeAndBounded(t *testing.T) {
	t.Run("route ID-name collision", func(t *testing.T) {
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			first := validCommandRoute("route_1", "route_2")
			second := validCommandRoute("route_2", "default")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{first, second}}})
		})
		_, err := resolveTunnelRoute(t.Context(), client, "tun_1", "route_2")
		var ambiguous *TunnelRouteSelectorAmbiguousError
		if !errors.As(err, &ambiguous) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("route duplicate and pagination", func(t *testing.T) {
		requests := 0
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests++
			route := validCommandRoute("route_1", "api")
			page := api.TunnelRoutePage{Items: []api.TunnelRoute{route}, NextCursor: "next"}
			if r.URL.Query().Get("cursor") == "next" {
				page.NextCursor = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": page})
		})
		route, err := resolveTunnelRoute(t.Context(), client, "tun_1", "api")
		if err != nil || route.ID != "route_1" || requests != 2 {
			t.Fatalf("route=%#v requests=%d error=%v", route, requests, err)
		}
	})
	t.Run("route cursor cycle", func(t *testing.T) {
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{NextCursor: "same"}})
		})
		_, err := resolveTunnelRoute(t.Context(), client, "tun_1", "missing")
		if !errors.Is(err, api.ErrUnsafeTunnelResponse) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("route ten-page fence", func(t *testing.T) {
		requests := 0
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{NextCursor: fmt.Sprintf("page-%d", requests)}})
		})
		_, err := resolveTunnelRoute(t.Context(), client, "tun_1", "missing")
		if !errors.Is(err, api.ErrUnsafeTunnelResponse) || requests != 10 {
			t.Fatalf("error=%v requests=%d", err, requests)
		}
	})
	t.Run("domain ID-hostname collision", func(t *testing.T) {
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			first := validCommandDomain("domain_1", "domain-2.example")
			second := validCommandDomain("domain-2.example", "app.example.test")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainPage{Items: []api.TunnelDomain{first, second}}})
		})
		_, err := resolveTunnelDomain(t.Context(), client, "tun_1", "domain-2.example")
		var ambiguous *TunnelDomainSelectorAmbiguousError
		if !errors.As(err, &ambiguous) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("domain pagination and cursor cycle", func(t *testing.T) {
		requests := 0
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests++
			page := api.TunnelDomainPage{NextCursor: "next"}
			if r.URL.Query().Get("cursor") == "next" {
				page.Items = []api.TunnelDomain{validCommandDomain("domain_1", "app.example.test")}
				page.NextCursor = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": page})
		})
		domain, err := resolveTunnelDomain(t.Context(), client, "tun_1", "APP.EXAMPLE.TEST")
		if err != nil || domain.ID != "domain_1" || requests != 2 {
			t.Fatalf("domain=%#v requests=%d error=%v", domain, requests, err)
		}
	})
	t.Run("domain cursor cycle", func(t *testing.T) {
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainPage{NextCursor: "same"}})
		})
		_, err := resolveTunnelDomain(t.Context(), client, "tun_1", "missing.example.test")
		if !errors.Is(err, api.ErrUnsafeTunnelResponse) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("domain ten-page fence", func(t *testing.T) {
		requests := 0
		client := tunnelSelectorTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainPage{NextCursor: fmt.Sprintf("page-%d", requests)}})
		})
		_, err := resolveTunnelDomain(t.Context(), client, "tun_1", "missing.example.test")
		if !errors.Is(err, api.ErrUnsafeTunnelResponse) || requests != 10 {
			t.Fatalf("error=%v requests=%d", err, requests)
		}
	})
}

func validCommandRoute(id, name string) api.TunnelRoute {
	return api.TunnelRoute{Schema: api.TunnelV1Schema, Kind: "route", ID: id, TunnelID: "tun_1", Name: name, Protocol: "http", HostMatch: api.TunnelRouteHostMatch{Type: "catch_all"}, Origin: api.TunnelRouteOrigin{Scheme: "http", Address: "127.0.0.1:8080", PreserveHost: true}, ConnectTimeoutMS: 10000, IdleTimeoutMS: 300000, MaxConcurrentStreams: 128, DesiredState: "active", Generation: 1, ETag: `"route:` + id + `:1"`}
}

func validCommandDomain(id, hostname string) api.TunnelDomain {
	return api.TunnelDomain{Schema: api.TunnelV1Schema, Kind: "domain_binding", ID: id, AccountID: "account_1", TunnelID: "tun_1", RouteID: "route_1", Hostname: hostname, MatchType: "exact", State: "ready", DNS: api.TunnelDomainDNS{Target: "edge.example.test"}, Certificate: api.TunnelDomainCertificate{State: "ready"}, Generation: 1, ETag: `"domain:` + id + `:1"`}
}

func TestTunnelNameSelectorResolvesToDurableID(t *testing.T) {
	var requests []string
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		tunnel := validCommandTunnel()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelPage{Items: []api.Tunnel{tunnel}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1":
			w.Header().Set("ETag", tunnel.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"show", "demo"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(requests, ",") != "GET /v1/tunnels,GET /v1/tunnels/tun_1" || !strings.Contains(output.String(), "tun_1") {
		t.Fatalf("requests=%v output=%q", requests, output.String())
	}
}

func TestTunnelDeleteRequiresConfirmationAndUsesCurrentETag(t *testing.T) {
	t.Setenv(config.EnvConfigPath, filepath.Join(t.TempDir(), "config.json"))
	requests := 0
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		tunnel := validCommandTunnel()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", tunnel.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
			return
		}
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s", r.Method)
		}
		if r.Header.Get("If-Match") != tunnel.ETag || r.Header.Get("Idempotency-Key") != "idem_test" {
			t.Fatalf("headers = %#v", r.Header)
		}
		tunnel.DesiredState = "deleted"
		_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
	})
	command := tunnelCobraCommandV1()
	var preview bytes.Buffer
	command.SetOut(&preview)
	command.SetArgs([]string{"delete", "tun_1"})
	if err := command.Execute(); err == nil || err.(exitCodeError).code != 2 {
		t.Fatalf("error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests before confirmation = %d", requests)
	}
	token := previewConfirmationCode(t, preview.String())
	var output bytes.Buffer
	command = tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"delete", "tun_1", "--confirm", token, "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d", requests)
	}
	if strings.Contains(strings.ToLower(output.String()), "secret") {
		t.Fatalf("unsafe output: %s", output.String())
	}
}

func TestTunnelCreateUsesCanonicalInputAndJSONOutput(t *testing.T) {
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
		if body.AccessMode != "private" || body.Origin.Scheme != "https" || body.Origin.Address != "origin.example:443" {
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
	command.SetArgs([]string{"create", "demo", "--from", "https://origin.example:443", "--private", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id":"tun_1"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestTunnelCreateContinuesFromPendingSelfhostEndpointToDNSInstructions(t *testing.T) {
	var requests []string
	domainCreates := 0
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels":
			tunnel := validCommandTunnel()
			tunnel.AccessMode = "public"
			tunnel.StableEndpoint = ""
			tunnel.SummaryCode = "custom_domain_required"
			w.Header().Set("ETag", tunnel.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: tunnel, Operation: validCommandOperation("tunnel", tunnel.ID)}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/routes":
			route := validCommandRoute("route_1", "default")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels/tun_1/domains":
			domainCreates++
			if domainCreates > 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"idempotency_conflict","message":"Creation cannot be repeated"}}`)
				return
			}
			var input api.TunnelDomainInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Hostname != "app.example.test" || input.RouteID != "route_1" {
				t.Fatalf("domain input = %#v", input)
			}
			domain := validCommandDomain("domain_1", input.Hostname)
			domain.State = "waiting_dns"
			domain.Certificate.State = "not_requested"
			w.Header().Set("ETag", domain.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelDomainMutation{Domain: domain, Operation: validCommandOperation("domain_binding", domain.ID), Changed: true}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/domains/domain_1":
			domain := validCommandDomain("domain_1", "app.example.test")
			domain.State = "waiting_dns"
			domain.Certificate.State = "not_requested"
			w.Header().Set("ETag", domain.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": domain})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/domains/domain_1/instructions":
			instructions := api.TunnelDNSInstructions{
				Schema: api.TunnelV1Schema, Kind: "dns_instructions", TunnelID: "tun_1", DomainID: "domain_1", Hostname: "app.example.test", Provider: "generic",
				Records: []api.TunnelDNSRecord{
					{Name: "app.example.test", Type: "A", Value: "203.0.113.10", TTL: 300},
					{Name: "app.example.test", Type: "AAAA", Value: "2001:db8::10", TTL: 300},
					{Name: "_acme-challenge.app.example.test", Type: "CNAME", Value: "pb.challenge.example.test", TTL: 300},
				},
				CertificateStrategy: "managed", VerificationState: "waiting_dns", Note: "Add these records.",
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": instructions})
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"create", "demo", "--port", "8080", "--domain", "app.example.test", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/tunnels",
		"GET /v1/tunnels/tun_1/routes",
		"POST /v1/tunnels/tun_1/domains",
		"GET /v1/tunnels/tun_1/domains/domain_1/instructions",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
	for _, expected := range []string{`"stable_endpoint":""`, `"type":"A"`, `"type":"AAAA"`, `"pb.challenge.example.test"`} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output missing %q: %s", expected, output.String())
		}
	}
	requests = nil
	retry := tunnelCobraCommandV1()
	retry.SetOut(&output)
	retry.SetArgs([]string{"create", "demo", "--port", "8080", "--domain", "app.example.test", "--json"})
	if err := retry.Execute(); err != nil {
		t.Fatal(err)
	}
	if domainCreates != 1 {
		t.Fatalf("domain creates=%d", domainCreates)
	}
	if strings.Join(requests, ",") != "POST /v1/tunnels,GET /v1/tunnels/tun_1/routes,GET /v1/tunnels/tun_1/domains/domain_1,GET /v1/tunnels/tun_1/domains/domain_1/instructions" {
		t.Fatalf("retry requests=%v", requests)
	}

}

func TestTunnelCreateHumanOutputExplainsPendingCustomerDomain(t *testing.T) {
	tunnel := validCommandTunnel()
	tunnel.StableEndpoint = ""
	tunnel.SummaryCode = "custom_domain_required"
	route := validCommandRoute("route_1", "default")
	route.TunnelID = tunnel.ID
	output := tunnelCreateHumanOutput(tunnelCreateOutput{
		Tunnel:    tunnel,
		Operation: validCommandOperation("tunnel", tunnel.ID),
		Route:     &route,
	}, "http://127.0.0.1:8080")
	if strings.Contains(output, " ->  http://") || !strings.Contains(output, "waiting for a verified customer domain") || !strings.Contains(output, "pb tunnel domain add tun_1 HOSTNAME --route route_1") {
		t.Fatalf("pending domain output = %q", output)
	}
}

func TestPublicTCPRouteRequiresVerifiedDomainWhenStableEndpointIsPending(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tunnels/tun_1" {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		tunnel := validCommandTunnel()
		tunnel.StableEndpoint = ""
		tunnel.SummaryCode = "custom_domain_required"
		w.Header().Set("ETag", tunnel.ETag)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"route", "add", "tun_1", "--name", "postgres", "--protocol", "tcp", "--to", "tcp://127.0.0.1:5432"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "verified customer domain") || !strings.Contains(err.Error(), "pb tunnel domain add tun_1 HOSTNAME") {
		t.Fatalf("error = %v", err)
	}
}

func TestTunnelCreateResolvesCanonicalOperationOnlyResponse(t *testing.T) {
	var requests []string
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels":
			operation := validCommandOperation("tunnel", "tun_1")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1":
			tunnel := validCommandTunnel()
			tunnel.AccessMode = "public"
			w.Header().Set("ETag", tunnel.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"create", "demo", "--port", "8080", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(requests, ","); got != "POST /v1/tunnels,GET /v1/tunnels/tun_1" {
		t.Fatalf("requests=%q", got)
	}
	if !strings.Contains(output.String(), `"kind":"tunnel_create"`) || !strings.Contains(output.String(), `"id":"tun_1"`) || !strings.Contains(output.String(), `"operation"`) {
		t.Fatalf("output=%s", output.String())
	}
}

func TestTunnelCreateReportsPartialDomainFailureAndUsesFreshKeys(t *testing.T) {
	var keys []string
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("Idempotency-Key"); key != "" {
			keys = append(keys, key)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels":
			tunnel := validCommandTunnel()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: tunnel, Operation: validCommandOperation("tunnel", tunnel.ID)}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1/routes":
			route := api.TunnelRoute{Schema: api.TunnelV1Schema, Kind: "route", ID: "route_1", TunnelID: "tun_1", Name: "default", Protocol: "http", HostMatch: api.TunnelRouteHostMatch{Type: "catch_all"}, Origin: api.TunnelRouteOrigin{Scheme: "http", Address: "127.0.0.1:8080", PreserveHost: true}, Priority: 100, ConnectTimeoutMS: 10000, IdleTimeoutMS: 300000, MaxConcurrentStreams: 128, DesiredState: "active", Generation: 1, ETag: `"route:route_1:1"`}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels/tun_1/domains":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"domain_conflict","message":"domain is already bound"}}`))
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	sequence := 0
	newTunnelIdempotencyKey = func() (string, error) { sequence++; return fmt.Sprintf("idem_%d", sequence), nil }
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"create", "demo", "--port", "8080", "--domain", "app.example.test"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "tunnel tun_1 was created") || !strings.Contains(err.Error(), "app.example.test") {
		t.Fatalf("error=%v", err)
	}
	if strings.Join(keys, ",") != "idem_1,idem_2" {
		t.Fatalf("keys=%v", keys)
	}
}

func TestTunnelCreateReplayEmitsOneCanonicalDocument(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		value := validCommandTunnel()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: value, Operation: validCommandOperation("tunnel", value.ID), Replayed: true}})
	})
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetArgs([]string{"create", "demo", "--port", "8080", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(output.String()), "\n") != 0 || !strings.Contains(output.String(), `"kind":"tunnel_create"`) || !strings.Contains(output.String(), `"replayed":true`) {
		t.Fatalf("output=%q", output.String())
	}
}

func TestTunnelCreateProcessRetryReportsExactExistingTunnel(t *testing.T) {
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"tunnel_name_conflict","message":"A tunnel with this name already exists in the account."}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels":
			existing := validCommandTunnel()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelPage{Items: []api.Tunnel{existing}}})
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"create", "demo", "--port", "8080"})
	err := command.Execute()
	var existing *TunnelCreateExistingError
	if !errors.As(err, &existing) || existing.TunnelID != "tun_1" || existing.Name != "demo" || existing.RecoveryCommand != "pb tunnel show tun_1" {
		t.Fatalf("error=%#v existing=%#v", err, existing)
	}
	if !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "pb tunnel show tun_1") {
		t.Fatalf("error=%v", err)
	}
}

func TestTunnelCreateInterruptedRetryReusesJournalAndCanonicalOutput(t *testing.T) {
	createCalls := 0
	var keys []string
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tunnels" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		createCalls++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		value := validCommandTunnel()
		operation := validCommandOperation("tunnel", value.ID)
		if createCalls == 1 {
			operation.State = "failed" // interrupts the first attempt after the journal records the tunnel
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: value, Operation: operation, Replayed: createCalls > 1, Changed: createCalls == 1}})
	})
	first := tunnelCobraCommandV1()
	first.SetArgs([]string{"create", "demo", "--port", "8080", "--json"})
	if err := first.Execute(); err == nil {
		t.Fatal("interrupted create unexpectedly succeeded")
	}
	var output bytes.Buffer
	second := tunnelCobraCommandV1()
	second.SetOut(&output)
	second.SetArgs([]string{"create", "demo", "--port", "8080", "--json"})
	if err := second.Execute(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if createCalls != 2 || len(keys) != 2 || keys[0] != keys[1] || keys[0] != "idem_test" {
		t.Fatalf("calls=%d keys=%v", createCalls, keys)
	}
	if strings.Count(strings.TrimSpace(output.String()), "\n") != 0 || !strings.Contains(output.String(), `"kind":"tunnel_create"`) || !strings.Contains(output.String(), `"replayed":true`) {
		t.Fatalf("output=%q", output.String())
	}
}

func TestTunnelCreateInterruptedRetryWithDifferentInputFailsBeforeNetwork(t *testing.T) {
	createCalls := 0
	withTunnelCommandClient(t, func(w http.ResponseWriter, _ *http.Request) {
		createCalls++
		value := validCommandTunnel()
		operation := validCommandOperation("tunnel", value.ID)
		operation.State = "failed" // interrupts the first attempt after the journal records the tunnel
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: value, Operation: operation, Changed: true}})
	})
	first := tunnelCobraCommandV1()
	first.SetArgs([]string{"create", "demo", "--port", "8080"})
	if err := first.Execute(); err == nil {
		t.Fatal("interrupted create unexpectedly succeeded")
	}
	second := tunnelCobraCommandV1()
	second.SetArgs([]string{"create", "demo", "--port", "9090"})
	err := second.Execute()
	if !errors.Is(err, tunnelcreatejournal.ErrRequestMismatch) || createCalls != 1 {
		t.Fatalf("error=%v create calls=%d", err, createCalls)
	}
}

func TestTunnelMutationWaitUsesBoundedOperationReadsAndSameMutationKey(t *testing.T) {
	oldInterval := tunnelOperationPollInterval
	tunnelOperationPollInterval = time.Millisecond
	t.Cleanup(func() { tunnelOperationPollInterval = oldInterval })
	operationReads := 0
	mutationCalls := 0
	withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
		tunnel := validCommandTunnel()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1":
			w.Header().Set("ETag", tunnel.ETag)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tunnels/tun_1/pause":
			mutationCalls++
			if r.Header.Get("Idempotency-Key") != "idem_test" || r.Header.Get("If-Match") != tunnel.ETag {
				t.Fatalf("headers=%v", r.Header)
			}
			tunnel.DesiredState = "paused"
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{Tunnel: tunnel, Operation: validCommandOperation("tunnel", tunnel.ID), Changed: true}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/operation_1":
			operationReads++
			operation := validCommandOperation("tunnel", tunnel.ID)
			operation.Phase, operation.State, operation.Progress = "ready", "succeeded", 100
			operation.UpdatedAt = time.Unix(2, 0).UTC()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
		default:
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
	})
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"pause", "tun_1", "--wait", "--timeout", "1s"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if mutationCalls != 1 || operationReads != 1 {
		t.Fatalf("mutation=%d operation reads=%d", mutationCalls, operationReads)
	}
}

func TestTunnelMutationRejectsInvalidWaitBeforeRequest(t *testing.T) {
	requests := 0
	withTunnelCommandClient(t, func(http.ResponseWriter, *http.Request) { requests++ })
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"pause", "tun_1", "--wait", "--timeout", "500ms"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "between 1s") {
		t.Fatalf("error=%v", err)
	}
	if requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestTunnelRoutePrivateTCPAndPathClearUseCanonicalWireShapes(t *testing.T) {
	t.Run("public tcp add", func(t *testing.T) {
		withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/tunnels/tun_1" {
				tunnel := validCommandTunnel()
				tunnel.AccessMode = "public"
				w.Header().Set("ETag", tunnel.ETag)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": tunnel})
				return
			}
			var input api.TunnelRouteInput
			if r.Method != http.MethodPost || r.URL.Path != "/v1/tunnels/tun_1/routes" || json.NewDecoder(r.Body).Decode(&input) != nil {
				t.Fatalf("request=%s %s", r.Method, r.URL.Path)
			}
			host := "123e4567-e89b-42d3-a456-426614174000.tunnels.example.test"
			if input.Protocol != "tcp" || input.HostMatch.Type != "managed_exact" || input.HostMatch.Hostname != host || input.Origin.Scheme != "tcp" {
				t.Fatalf("public TCP input=%+v", input)
			}
			route := validCommandRoute("route_tcp_1", "postgres")
			route.Protocol, route.HostMatch, route.Origin = "tcp", input.HostMatch, input.Origin
			route.PublicTCPListenerID, route.PublicTCPPort = "listener_1", 24567
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: route, Operation: validCommandOperation("route", route.ID), Changed: true}})
		})
		command := tunnelCobraCommandV1()
		command.SetArgs([]string{"route", "add", "tun_1", "--name", "postgres", "--protocol", "tcp", "--to", "tcp://127.0.0.1:5432", "--json"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("private tcp add", func(t *testing.T) {
		withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
			var input api.TunnelRouteInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Protocol != "tcp_private" || input.Origin.Scheme != "tcp" || input.Origin.Address != "127.0.0.1:22" || input.HostMatch.Type != "catch_all" {
				t.Fatalf("input=%#v", input)
			}
			route := api.TunnelRoute{Schema: api.TunnelV1Schema, Kind: "route", ID: "route_1", TunnelID: "tun_1", Name: "ssh", Protocol: "tcp_private", HostMatch: api.TunnelRouteHostMatch{Type: "catch_all"}, Origin: input.Origin, Priority: 0, ConnectTimeoutMS: 10000, IdleTimeoutMS: 300000, MaxConcurrentStreams: 128, DesiredState: "active", Generation: 1, ETag: `"route:route_1:1"`}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: route, Operation: validCommandOperation("route", route.ID), Changed: true}})
		})
		command := tunnelCobraCommandV1()
		command.SetArgs([]string{"route", "add", "tun_1", "--name", "ssh", "--protocol", "tcp_private", "--to", "tcp://127.0.0.1:22"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("clear path", func(t *testing.T) {
		withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
			route := api.TunnelRoute{Schema: api.TunnelV1Schema, Kind: "route", ID: "route_1", TunnelID: "tun_1", Name: "api", Protocol: "http", HostMatch: api.TunnelRouteHostMatch{Type: "catch_all"}, PathPrefix: stringPointer("/api"), Origin: api.TunnelRouteOrigin{Scheme: "http", Address: "127.0.0.1:8080", PreserveHost: true}, Priority: 0, ConnectTimeoutMS: 10000, IdleTimeoutMS: 300000, MaxConcurrentStreams: 128, DesiredState: "active", Generation: 1, ETag: `"route:route_1:1"`}
			if r.Method == http.MethodGet {
				if r.URL.Path != "/v1/tunnels/tun_1/routes" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRoutePage{Items: []api.TunnelRoute{route}}})
				return
			}
			if r.Method != http.MethodPatch || r.URL.Path != "/v1/tunnels/tun_1/routes/route_1" {
				t.Fatalf("request=%s %s", r.Method, r.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if value, present := body["path_prefix"]; !present || value != nil {
				t.Fatalf("body=%#v", body)
			}
			route.PathPrefix = nil
			route.Generation = 2
			route.ETag = `"route:route_1:2"`
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelRouteMutation{Route: route, Operation: validCommandOperation("route", route.ID), Changed: true}})
		})
		command := tunnelCobraCommandV1()
		command.SetArgs([]string{"route", "update", "tun_1", "route_1", "--clear-path"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
	})
}

func stringPointer(value string) *string { return &value }
