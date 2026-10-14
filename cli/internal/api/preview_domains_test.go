package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestNormalizePreviewDomainsCanonicalizesIDNAAndOrdering(t *testing.T) {
	got, err := NormalizePreviewDomains([]string{"*.BÜCHER.Example.", "EXAMPLE.COM.", "app.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"*.xn--bcher-kva.example", "app.example.com", "example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domains = %#v, want %#v", got, want)
	}
	for _, test := range []struct {
		name  string
		input []string
		isErr error
	}{
		{name: "normalized duplicate", input: []string{"Example.com", "example.com."}, isErr: ErrPreviewLeaseInvalid},
		{name: "recursive wildcard", input: []string{"*.*.example.com"}, isErr: ErrPreviewLeaseInvalid},
		{name: "wildcard not first label", input: []string{"app.*.example.com"}, isErr: ErrPreviewLeaseInvalid},
		{name: "ip literal", input: []string{"127.0.0.1"}, isErr: ErrPreviewLeaseInvalid},
		{name: "local name", input: []string{"localhost"}, isErr: ErrPreviewLeaseInvalid},
		{name: "underscore label", input: []string{"app_name.example.com"}, isErr: ErrPreviewLeaseInvalid},
		{name: "surrounding whitespace", input: []string{" app.example.com"}, isErr: ErrPreviewLeaseInvalid},
		{name: "unicode whitespace", input: []string{"app.\u00a0example.com"}, isErr: ErrPreviewLeaseInvalid},
		{name: "unicode control", input: []string{"app.\u007f.example.com"}, isErr: ErrPreviewLeaseInvalid},
		{name: "too many", input: []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com", "g.example.com", "h.example.com", "i.example.com"}, isErr: ErrPreviewLeaseInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizePreviewDomains(test.input); !errors.Is(err, test.isErr) {
				t.Fatalf("error = %v, want %v", err, test.isErr)
			}
		})
	}
}

func TestPreviewLeaseIdempotencyKeyIsStrictAndBounded(t *testing.T) {
	valid := "preview_01ABCxyz-_.~"
	if err := validatePreviewLeaseIdempotencyKey(valid); err != nil {
		t.Fatalf("valid key error = %v", err)
	}
	for _, value := range []string{
		"", " key", "key ", "key\tvalue", "key\u00a0value", "key\u007fvalue",
		strings.Repeat("k", 257),
	} {
		if err := validatePreviewLeaseIdempotencyKey(value); err == nil {
			t.Fatalf("key %q was accepted", value)
		}
	}
}

func TestCreatePreviewLeaseSendsOrderedDomainsAndSafeSummaries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	lease := PreviewLease{
		Schema: PreviewTunnelSchemaV1, Kind: "preview_lease", ID: "prv_domains", AccountID: "acct_1", ActorID: "actor_1",
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground", Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		AccessMode: "public", Endpoint: "https://preview.example.test", EndpointState: "ready", LeaseDeadline: now.Add(time.Hour), State: "connecting",
		AllocationState: "pending", EdgeState: "pending", OriginState: "unknown", CreatedAt: now, LastRenewedAt: now,
		Domains: []PreviewDomainSummary{{
			ID: "dom_1", TargetKind: "preview_lease", PreviewID: "prv_domains", Hostname: "app.example.com", MatchType: "exact", State: "waiting_dns",
			DNS: PreviewDomainDNS{Target: "domains.example.test"}, Certificate: PreviewDomainCertificate{State: "not_requested"}, Generation: 1, ETag: `"dom_1:1"`,
			Instructions: &PreviewDNSInstructions{
				Schema: PreviewTunnelSchemaV1, Kind: "dns_instructions", TargetKind: "preview_lease", PreviewID: "prv_domains", DomainID: "dom_1", Hostname: "app.example.com", Provider: "generic",
				Records: []PreviewDNSRecord{{Name: "_acme-challenge.app.example.com", Type: "CNAME", Value: "pb-1.challenge.example.test", TTL: 300}}, CertificateStrategy: "delegated_dns01", VerificationState: "waiting_dns", Note: "Add this CNAME.",
			},
		}, {
			ID: "dom_2", TargetKind: "preview_lease", PreviewID: "prv_domains", Hostname: "example.com", MatchType: "exact", State: "waiting_dns",
			DNS: PreviewDomainDNS{Target: "domains.example.test"}, Certificate: PreviewDomainCertificate{State: "not_requested"}, Generation: 1, ETag: `"dom_2:1"`,
		}},
	}
	var request PreviewLeaseCreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/previews" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2X2RvbWFpbnM:1"`)
		w.Header().Set("X-Paperboat-Operation-ID", "op_domains")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": lease})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "access-token"}, server.Client())
	got, err := client.CreatePreviewLease(context.Background(), PreviewLeaseCreateRequest{
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground", Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, Domains: []string{"EXAMPLE.COM.", "app.example.com"},
	}, "create-domains")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request.Domains, []string{"app.example.com", "example.com"}) {
		t.Fatalf("request domains = %#v", request.Domains)
	}
	if len(got.Domains) != 2 || got.Domains[0].TargetKind != "preview_lease" || got.Domains[0].PreviewID != got.ID || got.Domains[0].Instructions == nil {
		t.Fatalf("lease domains = %#v", got.Domains)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(strings.ToLower(string(encoded)), "token") || strings.Contains(strings.ToLower(string(encoded)), "private_key") {
		t.Fatalf("lease projection contains secret material: %s", encoded)
	}
}

func TestPreviewLeaseRejectsMixedDomainTargetBinding(t *testing.T) {
	now := time.Now().UTC()
	lease := PreviewLease{
		Schema: PreviewTunnelSchemaV1, Kind: "preview_lease", ID: "prv_1", AccountID: "acct_1", ActorID: "actor_1", OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public", Endpoint: "https://preview.example.test", EndpointState: "ready", LeaseDeadline: now.Add(time.Hour), State: "ready", AllocationState: "ready", EdgeState: "ready", OriginState: "ready", CreatedAt: now, LastRenewedAt: now,
		Domains: []PreviewDomainSummary{{ID: "dom_1", TargetKind: "tunnel_route", PreviewID: "prv_1", Hostname: "app.example.com", MatchType: "exact", State: "ready", DNS: PreviewDomainDNS{Target: "dns.example.test"}, Certificate: PreviewDomainCertificate{State: "ready"}, Generation: 1, ETag: `"dom_1:1"`}},
	}
	if err := validatePreviewLease(lease); !errors.Is(err, ErrPreviewLeaseInvalid) {
		t.Fatalf("error = %v, want ErrPreviewLeaseInvalid", err)
	}
}

func TestGetPreviewDomainDNSInstructionsUsesRuntimePathAndBindsIdentity(t *testing.T) {
	runtimePrefix := "/v1/runtime/customer.example.test"
	returnedDomainID := "domain_1"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodGet || r.URL.Path != runtimePrefix+"/v1/previews/preview_1/domains/domain_1/instructions" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		writePreviewLeaseEnvelope(t, w, PreviewDNSInstructions{
			Schema: PreviewTunnelSchemaV1, Kind: "dns_instructions", TargetKind: "preview_lease", PreviewID: "preview_1", DomainID: returnedDomainID,
			Hostname: "app.example.test", Provider: "generic",
			Records: []PreviewDNSRecord{
				{Name: "app.example.test", Type: "A", Value: "203.0.113.10", TTL: 300},
				{Name: "app.example.test", Type: "A", Value: "198.51.100.20", TTL: 300},
				{Name: "app.example.test", Type: "AAAA", Value: "2001:db8::10", TTL: 300},
				{Name: "app.example.test", Type: "AAAA", Value: "2001:db8::20", TTL: 300},
				{Name: "_acme-challenge.app.example.test", Type: "CNAME", Value: "pb.challenge.example.test", TTL: 300},
			},
			CertificateStrategy: "delegated_dns01", VerificationState: "waiting_dns", Note: "Add this CNAME.",
		})
	}))
	defer server.Close()
	client := New(server.URL+runtimePrefix, config.Credential{AccessToken: "access-token"}, server.Client())
	instructions, err := client.GetPreviewDomainDNSInstructions(context.Background(), "preview_1", "domain_1")
	if err != nil {
		t.Fatal(err)
	}
	if instructions.Hostname != "app.example.test" || len(instructions.Records) != 5 || instructions.Records[0].Type != "A" || instructions.Records[2].Type != "AAAA" || instructions.Records[4].Value != "pb.challenge.example.test" {
		t.Fatalf("instructions = %#v", instructions)
	}
	returnedDomainID = "domain_other"
	if _, err := client.GetPreviewDomainDNSInstructions(context.Background(), "preview_1", "domain_1"); !errors.Is(err, ErrPreviewLeaseInvalid) {
		t.Fatalf("mismatched domain binding error = %v, want ErrPreviewLeaseInvalid", err)
	}
	if !reflect.DeepEqual(paths, []string{
		"GET " + runtimePrefix + "/v1/previews/preview_1/domains/domain_1/instructions",
		"GET " + runtimePrefix + "/v1/previews/preview_1/domains/domain_1/instructions",
	}) {
		t.Fatalf("request paths = %#v", paths)
	}
}

func TestValidatePreviewDNSInstructionsRejectsMalformedOrUnboundRecords(t *testing.T) {
	lease := PreviewLease{ID: "preview_1"}
	domain := PreviewDomainSummary{ID: "domain_1", Hostname: "app.example.test"}
	valid := PreviewDNSInstructions{
		Schema: PreviewTunnelSchemaV1, Kind: "dns_instructions", TargetKind: "preview_lease", PreviewID: lease.ID, DomainID: domain.ID,
		Hostname: domain.Hostname, Provider: "generic", CertificateStrategy: "managed", VerificationState: "waiting_dns", Note: "Add these records.",
		Records: []PreviewDNSRecord{
			{Name: domain.Hostname, Type: "A", Value: "203.0.113.10", TTL: 300},
			{Name: domain.Hostname, Type: "A", Value: "198.51.100.20", TTL: 300},
			{Name: domain.Hostname, Type: "AAAA", Value: "2001:db8::10", TTL: 300},
			{Name: domain.Hostname, Type: "AAAA", Value: "2001:db8::20", TTL: 300},
			{Name: "_acme-challenge.app.example.test", Type: "CNAME", Value: "pb.challenge.example.test", TTL: 300},
		},
	}
	if err := validatePreviewDNSInstructions(lease, domain, valid); err != nil {
		t.Fatalf("valid selected-edge and challenge records rejected: %v", err)
	}

	for name, mutate := range map[string]func(*PreviewDNSInstructions){
		"A record with IPv6 value":            func(value *PreviewDNSInstructions) { value.Records[0].Value = "2001:db8::10" },
		"AAAA record with IPv4 value":         func(value *PreviewDNSInstructions) { value.Records[2].Value = "203.0.113.10" },
		"address record for another hostname": func(value *PreviewDNSInstructions) { value.Records[0].Name = "other.example.test" },
		"unbound CNAME name":                  func(value *PreviewDNSInstructions) { value.Records[4].Name = "_acme-challenge.other.example.test" },
		"invalid CNAME target":                func(value *PreviewDNSInstructions) { value.Records[4].Value = "bad/target" },
		"unsupported record type":             func(value *PreviewDNSInstructions) { value.Records[4].Type = "TXT" },
		"duplicate record":                    func(value *PreviewDNSInstructions) { value.Records[1] = value.Records[0] },
		"more than two-edge address bound": func(value *PreviewDNSInstructions) {
			value.Records = append(value.Records, PreviewDNSRecord{Name: domain.Hostname, Type: "A", Value: "192.0.2.30", TTL: 300})
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			value.Records = append([]PreviewDNSRecord(nil), valid.Records...)
			mutate(&value)
			if err := validatePreviewDNSInstructions(lease, domain, value); err == nil {
				t.Fatal("malformed DNS records were accepted")
			}
		})
	}
}
