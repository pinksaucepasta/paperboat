package tunnelv1

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

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
	if err := domainClaimError(dbsqlc.TunnelDomain{}, "wsd_a", now); !errors.Is(err, ErrDomainConflict) || errors.Is(err, ErrDomainQuarantined) {
		t.Fatalf("live claim must remain exclusive across accounts: %v", err)
	}
	deleted := dbsqlc.TunnelDomain{DeletedAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}, QuarantineUntil: sql.NullTime{Time: now.Add(time.Hour), Valid: true},
		WorkspaceDomainID: sql.NullString{String: "wsd_old", Valid: true}}
	if err := domainClaimError(deleted, "wsd_a", now); !errors.Is(err, ErrDomainConflict) || !errors.Is(err, ErrDomainQuarantined) {
		t.Fatalf("active quarantine must block reuse under another workspace domain and say why: %v", err)
	}
	if err := domainClaimError(deleted, "wsd_old", now); err != nil {
		t.Fatalf("a hostname removed under the same workspace domain must be free again at once: %v", err)
	}
	deleted.QuarantineUntil.Time = now
	if err := domainClaimError(deleted, "wsd_a", now); err != nil {
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
