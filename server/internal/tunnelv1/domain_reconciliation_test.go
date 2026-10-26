package tunnelv1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestDomainDNSObservationRequiresExactStableTarget(t *testing.T) {
	row := dbsqlc.TunnelDomain{DnsTarget: "stable.edge.paperboat.example"}
	for _, test := range []struct {
		name        string
		recordType  string
		observation DNSObservation
		want        bool
	}{
		{name: "exact cname", recordType: "CNAME", observation: DNSObservation{Records: []string{"CNAME stable.edge.paperboat.example"}}, want: true},
		{name: "terminal dot normalized", recordType: "CNAME", observation: DNSObservation{Records: []string{"cname STABLE.EDGE.PAPERBOAT.EXAMPLE"}}, want: true},
		{name: "wrong cname", recordType: "CNAME", observation: DNSObservation{Records: []string{"CNAME attacker.example"}}},
		{name: "resolver failure", recordType: "CNAME", observation: DNSObservation{Records: []string{"CNAME stable.edge.paperboat.example"}, FailureCode: "dnssec_error"}},
		{name: "flattened compared by resolver", recordType: "ALIAS", observation: DNSObservation{Records: []string{"A 192.0.2.1"}}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := dnsObservationMatches(row, test.observation, test.recordType); got != test.want {
				t.Fatalf("match = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNetDomainDNSResolverUsesWildcardProbeAndAuthoritativeTTL(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		question := request.Question[0]
		if strings.HasPrefix(question.Name, "paperboat-") && strings.HasSuffix(question.Name, ".example.com.") && question.Qtype == dns.TypeCNAME {
			response.Answer = append(response.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 900}, Target: "stable.paperboat.example."})
		} else {
			response.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(response)
	})
	server := &dns.Server{PacketConn: packet, Handler: handler}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })

	resolver := NetDomainDNSResolver{Servers: []string{packet.LocalAddr().String()}, Timeout: time.Second}
	observation, err := resolver.Observe(context.Background(), "*.example.com", "CNAME", "stable.paperboat.example", "dns-challenge://test", nil)
	if err != nil || observation.FailureCode != "" || observation.TTL != 15*time.Minute || len(observation.Records) != 1 {
		t.Fatalf("observation = %+v, %v", observation, err)
	}
}

func TestDomainDNSBackoffRespectsTTLAndCaps(t *testing.T) {
	if got := domainDNSBackoff(0, 5*time.Minute, false); got != 5*time.Minute {
		t.Fatalf("TTL delay = %v", got)
	}
	if got := domainDNSBackoff(30, 30*time.Second, false); got != 30*time.Minute {
		t.Fatalf("capped delay = %v", got)
	}
	if got := domainDNSBackoff(30, 2*time.Hour, true); got != 2*time.Hour {
		t.Fatalf("verified recheck = %v", got)
	}
	if got := domainDNSBackoff(0, 0, true); got != domainDNSDefaultTTL {
		t.Fatalf("verified recheck with a zero TTL = %v", got)
	}
}

func TestVerifiedDomainDNSDriftAndTransientGrace(t *testing.T) {
	row := dbsqlc.TunnelDomain{OwnershipState: "verified", ConflictState: "clear", DnsTarget: "stable.edge.paperboat.example"}
	state, conflict, verified := domainDNSObservationState(row, DNSObservation{FailureCode: "dns_unavailable"}, "CNAME", context.DeadlineExceeded)
	if state != "verified" || conflict != "clear" || verified {
		t.Fatalf("first transient = %q %q %v", state, conflict, verified)
	}
	row.VerificationAttempts = domainDNSTransientFailureGrace - 1
	state, conflict, verified = domainDNSObservationState(row, DNSObservation{FailureCode: "dns_unavailable"}, "CNAME", context.DeadlineExceeded)
	if state != "failed" || conflict != "clear" || verified {
		t.Fatalf("expired grace = %q %q %v", state, conflict, verified)
	}
	row.VerificationAttempts = 0
	state, conflict, verified = domainDNSObservationState(row, DNSObservation{FailureCode: "wrong_record", Records: []string{"CNAME attacker.example"}}, "CNAME", nil)
	if state != "failed" || conflict != "conflicted" || verified {
		t.Fatalf("wrong target = %q %q %v", state, conflict, verified)
	}
	state, conflict, verified = domainDNSObservationState(row, DNSObservation{FailureCode: "dns_not_found"}, "CNAME", &net.DNSError{IsNotFound: true})
	if state != "failed" || conflict != "clear" || verified {
		t.Fatalf("removed record = %q %q %v", state, conflict, verified)
	}
}

func TestDomainLifecycleReportsIssuingTLSAfterDNSVerification(t *testing.T) {
	row := dbsqlc.TunnelDomain{OwnershipState: "verified", ConflictState: "clear", CertificateState: "issuing"}
	if got := wireDomainLifecycleState(row); got != "issuing_tls" {
		t.Fatalf("state = %q", got)
	}
	row.OwnershipState = "failed"
	if got := wireDomainLifecycleState(row); got != "dns_error" {
		t.Fatalf("drifted state = %q", got)
	}
}

func TestDomainDNSInstructionsAreProviderAwareAndOriginOpaque(t *testing.T) {
	for _, test := range []struct {
		host, provider, wantType string
	}{
		{host: "app.example.com", provider: "generic", wantType: "CNAME"},
		{host: "*.example.com", provider: "cloudflare", wantType: "CNAME"},
		{host: "example.com", provider: "cloudflare", wantType: "CNAME"},
		{host: "example.com", provider: "route53", wantType: "ALIAS"},
		{host: "example.com", provider: "generic", wantType: "ANAME"},
	} {
		recordType, note := dnsRecordTypeAndNote(test.host, test.provider)
		if recordType != test.wantType || note == "" {
			t.Fatalf("%s/%s = %s %q", test.host, test.provider, recordType, note)
		}
	}
	records, err := json.Marshal([]DNSRecordInstruction{{Name: "*.example.com", Type: "CNAME", Value: "stable.paperboat.example", TTL: 300}})
	if err != nil || expectedDNSRecordType(records) != "CNAME" {
		t.Fatalf("expected record = %s, %v", records, err)
	}
}

func TestDNSProviderValidationFailsClosed(t *testing.T) {
	for _, provider := range []string{"", "generic", "cloudflare", "route53", "google_cloud_dns", "digitalocean", "namecheap"} {
		if _, err := validateDNSProvider(provider); err != nil {
			t.Fatalf("provider %q: %v", provider, err)
		}
	}
	if _, err := validateDNSProvider("unknown-provider"); err == nil {
		t.Fatal("unknown provider was accepted")
	}
}

func TestOneLabelWildcardDoesNotCaptureApexOrMultipleLabels(t *testing.T) {
	hostname, matchType, err := normalizeBindingHostname("*.Example.COM.")
	if err != nil || hostname != "*.example.com" || matchType != "one_label_wildcard" {
		t.Fatalf("normalization = %q %q %v", hostname, matchType, err)
	}
	match := func(candidate string) bool {
		suffix := strings.TrimPrefix(hostname, "*.")
		prefix, ok := strings.CutSuffix(candidate, "."+suffix)
		return ok && prefix != "" && !strings.Contains(prefix, ".")
	}
	if !match("app.example.com") || match("example.com") || match("a.b.example.com") {
		t.Fatal("wildcard did not preserve one-label semantics")
	}
}

func TestDomainClaimQuarantineIsBoundedAndLiveClaimsStayExclusive(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := domainClaimError(dbsqlc.TunnelDomain{}, now); !errors.Is(err, ErrDomainConflict) || errors.Is(err, ErrDomainQuarantined) {
		t.Fatalf("live claim must remain exclusive across accounts: %v", err)
	}
	deleted := dbsqlc.TunnelDomain{DeletedAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}, QuarantineUntil: sql.NullTime{Time: now.Add(time.Hour), Valid: true}}
	if err := domainClaimError(deleted, now); !errors.Is(err, ErrDomainConflict) || !errors.Is(err, ErrDomainQuarantined) {
		t.Fatalf("active quarantine must block reuse and say why: %v", err)
	}
	deleted.QuarantineUntil.Time = now
	if err := domainClaimError(deleted, now); err != nil {
		t.Fatalf("expired quarantine must allow a fresh independently verified claim: %v", err)
	}
}

func TestDomainViewIncludesEmptyObservedRecordsArray(t *testing.T) {
	view := DomainViewFromRow(resourceDomain("dom_empty", "app.example.test", "pending", "clear"))
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if view.DNS.ObservedRecords == nil || !strings.Contains(string(encoded), `"observed_records":[]`) {
		t.Fatalf("empty DNS observation is not an array: %s", encoded)
	}
}

func TestNetDomainDNSResolverComparesLiteralAddressTargets(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var recovered atomic.Bool
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		q := request.Question[0]
		if q.Name == "mixed.example.test." && !recovered.Load() {
			response.Rcode = dns.RcodeNameError
			if q.Qtype == dns.TypeAAAA {
				response.Rcode = dns.RcodeServerFailure
			}
			if err := w.WriteMsg(response); err != nil {
				t.Errorf("write DNS response: %v", err)
			}
			return
		}
		switch {
		case (q.Name == "v4.example.test." || q.Name == "mixed.example.test.") && q.Qtype == dns.TypeA:
			response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.ParseIP("140.245.11.41")}}
		case q.Name == "pair.example.test." && q.Qtype == dns.TypeA:
			for _, address := range []string{"140.245.11.41", "152.67.0.60"} {
				response.Answer = append(response.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.ParseIP(address)})
			}
		case q.Name == "v6.example.test." && q.Qtype == dns.TypeAAAA:
			response.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 120}, AAAA: net.ParseIP("2001:db8::1")}}
		}
		_ = w.WriteMsg(response)
	})
	server := &dns.Server{PacketConn: packet, Handler: handler}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	resolver := NetDomainDNSResolver{Servers: []string{packet.LocalAddr().String()}, Timeout: time.Second}
	for _, c := range []struct{ host, kind, target, failure string }{
		{"v4.example.test", "A", "140.245.11.41", ""},
		{"v4.example.test", "A", "152.67.0.60", "wrong_record"},
		{"v6.example.test", "AAAA", "2001:db8::1", ""},
		{"v6.example.test", "AAAA", "2001:db8::2", "wrong_record"},
	} {
		observation, err := resolver.Observe(t.Context(), c.host, c.kind, c.target, "", nil)
		if err != nil || observation.FailureCode != c.failure {
			t.Fatalf("%+v: %+v %v", c, observation, err)
		}
	}
	for _, c := range []struct {
		records string
		want    string
	}{
		{`[{"type":"A","value":"140.245.11.41"},{"type":"A","value":"152.67.0.60"}]`, ""},
		{`[{"type":"A","value":"140.245.11.41"}]`, "wrong_record"},
		{`[{"type":"A","value":"140.245.11.41"},{"type":"A","value":"152.67.0.61"}]`, "wrong_record"},
	} {
		observation, err := resolver.Observe(t.Context(), "pair.example.test", "A", "140.245.11.41", "", []byte(c.records))
		if err != nil || observation.FailureCode != c.want {
			t.Fatalf("two-edge DNS records %s: %+v %v", c.records, observation, err)
		}
	}
	observation, failure := resolver.Observe(t.Context(), "mixed.example.test", "A", "140.245.11.41", "", nil)
	if failure == nil || observation.FailureCode == "dns_not_found" {
		t.Fatalf("mixed DNS absence and server failure must remain operational: %+v %v", observation, failure)
	}
	var codes []string
	var inspect func(error)
	inspect = func(err error) {
		if d, ok := err.(*net.DNSError); ok {
			codes = append(codes, d.Err)
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				inspect(child)
			}
		} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			inspect(wrapped.Unwrap())
		}
	}
	inspect(failure)
	if len(codes) != 2 || codes[0] != "NXDOMAIN" || codes[1] != "SERVFAIL" {
		t.Fatalf("original DNS failures lost: %v", codes)
	}
	recovered.Store(true)
	observation, failure = resolver.Observe(t.Context(), "mixed.example.test", "A", "140.245.11.41", "", nil)
	if failure != nil || observation.FailureCode != "" {
		t.Fatalf("same resolver did not recover: %+v %v", observation, failure)
	}
	observation, failure = resolver.Observe(t.Context(), "missing.example.test", "A", "140.245.11.41", "", nil)
	if failure == nil || observation.FailureCode != "dns_not_found" {
		t.Fatalf("pure two-family absence became operational: %+v %v", observation, failure)
	}

}
