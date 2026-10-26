package previewdomain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
)

func TestNormalizeHostnameCanonicalizesExactAndOneLabelWildcard(t *testing.T) {
	cases := []struct {
		name, input, want, match string
	}{
		{"exact", " Example.COM. ", "example.com", "exact"},
		{"unicode", "Bücher.Example", "xn--bcher-kva.example", "exact"},
		{"wildcard", "*.Apps.Example.", "*.apps.example", "one_label_wildcard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, match, err := NormalizeHostname(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || match != tc.match {
				t.Fatalf("NormalizeHostname(%q) = %q, %q; want %q, %q", tc.input, got, match, tc.want, tc.match)
			}
		})
	}
	for _, input := range []string{"example", "*.com", "https://example.com", "a..example.com", "*.a.*.example.com"} {
		t.Run("invalid_"+input, func(t *testing.T) {
			if _, _, err := NormalizeHostname(input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("NormalizeHostname(%q) error = %v, want ErrInvalidInput", input, err)
			}
		})
	}
}

func TestServiceCreateAndReadFencesPreviewLease(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryFake{lease: LeaseContext{ID: "preview-1", AccountID: "account-1", Generation: 4, LeaseDeadline: now.Add(time.Hour), TerminalState: "active", OwnerMachineID: "machine-1", Endpoint: "https://preview.example"}}
	service, err := NewService(repository, Config{CursorKey: bytes32(7), Now: func() time.Time { return now }, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte(`{"hostname":"alias.example"}`))
	request := previewtunnelapi.RequestContext{Actor: previewtunnelapi.Actor{AccountID: "account-1", ActorID: "user-1", MachineID: "machine-1", Role: "user", Scopes: []string{"previews:read", "previews:write"}}, RequestID: "request-1", CorrelationID: "correlation-1"}
	result, err := service.Create(context.Background(), request, "preview-1", Request{Hostname: "Alias.Example.", Provider: "generic", Mutation: MutationInput{IdempotencyKey: "idempotency-1", RequestHash: requestHash}})
	if err != nil {
		t.Fatal(err)
	}
	if repository.create.Hostname != "alias.example" || repository.create.PreviewGeneration != 4 || repository.create.DNSTarget != "preview.example" {
		t.Fatalf("create record = %#v", repository.create)
	}
	if result.Domain.Hostname != "alias.example" || result.Operation.ID == "" {
		t.Fatalf("create result = %#v", result)
	}
	repository.lease.Generation = 5
	if _, err := service.Get(context.Background(), request, "preview-1", result.Domain.ID); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale generation error = %v", err)
	}
	repository.lease.TerminalState = "expired"
	if _, err := service.List(context.Background(), request, "preview-1", "", 1); !errors.Is(err, ErrLeaseNotActive) {
		t.Fatalf("expired lease error = %v", err)
	}
}

func TestServiceProjectsListGetRenewStopAndReadyAliases(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	lease := LeaseContext{
		ID: "preview-1", AccountID: "account-1", Generation: 1,
		LeaseDeadline: now.Add(time.Hour), TerminalState: "active",
		OwnerMachineID: "machine-1", OwnerSessionID: "session-1", Endpoint: "https://preview.example",
	}
	exact := previewDomainRow("domain-exact", "account-1", "preview-1", "alias.example", "exact", 1, now)
	wildcard := previewDomainRow("domain-wildcard", "account-1", "preview-1", "*.apps.example", "one_label_wildcard", 1, now)
	stale := previewDomainRow("domain-stale", "account-1", "preview-1", "stale.example", "exact", 0, now)
	repository := &serviceRepositoryFake{
		lease: lease,
		list:  []dbsqlc.PreviewDomain{exact, wildcard, stale},
		get:   wildcard,
		ready: []ReadyAliasRecord{
			{Domain: exact, CertificateReference: "cert-exact", CertificateGeneration: 17},
			{Domain: wildcard, CertificateReference: "cert-wildcard", CertificateGeneration: 23},
			{Domain: stale, CertificateReference: "cert-stale", CertificateGeneration: 99},
		},
	}
	service, err := NewService(repository, Config{CursorKey: bytes32(9), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := previewtunnelapi.RequestContext{Actor: previewtunnelapi.Actor{
		AccountID: "account-1", ActorID: "user-1", MachineID: "machine-1", Role: "user",
		Scopes: []string{"previews:read", "previews:write"},
	}}

	page, err := service.List(context.Background(), request, "preview-1", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Hostname != "alias.example" || page.Items[1].Hostname != "*.apps.example" {
		t.Fatalf("list projection = %#v, want current-generation domains only", page.Items)
	}
	if page.Items[1].WildcardLabels == nil || *page.Items[1].WildcardLabels != 1 || page.Items[1].Certificate.Reference != "cert-ref" {
		t.Fatalf("wildcard list projection = %#v", page.Items[1])
	}

	got, err := service.Get(context.Background(), request, "preview-1", wildcard.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != wildcard.Hostname || got.ETag != previewtunnelapi.ETag(Kind, wildcard.ID, wildcard.Generation) || got.WildcardLabels == nil {
		t.Fatalf("get projection = %#v", got)
	}

	aliases, err := service.ReadyAliases(context.Background(), request, "preview-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 {
		t.Fatalf("ready aliases = %#v, want current certificate-ready records", aliases)
	}
	if aliases[0].CertificateGeneration != 17 || aliases[1].CertificateGeneration != 23 || aliases[1].WildcardLabels == nil || *aliases[1].WildcardLabels != 1 {
		t.Fatalf("ready alias certificate projection = %#v", aliases)
	}

	// Renewal advances the lease and every live alias target atomically. A
	// service read must project the new preview generation and retain the
	// independent certificate generation/reference unchanged.
	repository.lease.Generation = 2
	for index := range repository.list {
		if repository.list[index].PreviewGeneration == 1 {
			repository.list[index].PreviewGeneration = 2
		}
	}
	repository.get.PreviewGeneration = 2
	for index := range repository.ready {
		if repository.ready[index].Domain.PreviewGeneration == 1 {
			repository.ready[index].Domain.PreviewGeneration = 2
		}
	}
	renewed, err := service.List(context.Background(), request, "preview-1", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(renewed.Items) != 2 || renewed.Items[0].PreviewID != "preview-1" || renewed.Items[0].Generation != exact.Generation {
		t.Fatalf("renewed list projection = %#v", renewed.Items)
	}
	renewedAliases, err := service.ReadyAliases(context.Background(), request, "preview-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(renewedAliases) != 2 || renewedAliases[0].PreviewGeneration != 2 || renewedAliases[0].CertificateGeneration != 17 {
		t.Fatalf("renewed alias projection = %#v", renewedAliases)
	}

	// Stop/expiry makes the lease unreadable for this live resource and the
	// withdrawn row itself projects as quarantined, never ready.
	withdrawn := exact
	withdrawn.DeletedAt = sql.NullTime{Time: now, Valid: true}
	withdrawn.OwnershipState = "revoked"
	withdrawn.CertificateState = "revoked"
	withdrawn.ConflictState = "quarantined"
	withdrawn.QuarantineUntil = sql.NullTime{Time: now.Add(Quarantine), Valid: true}
	if view := DomainViewFromRow(withdrawn); view.State != "quarantined" {
		t.Fatalf("withdrawn domain state = %q, want quarantined", view.State)
	}
	repository.lease.TerminalState = "stopped"
	if _, err := service.List(context.Background(), request, "preview-1", "", 10); !errors.Is(err, ErrLeaseNotActive) {
		t.Fatalf("stopped list error = %v", err)
	}
	if _, err := service.Get(context.Background(), request, "preview-1", wildcard.ID); !errors.Is(err, ErrLeaseNotActive) {
		t.Fatalf("stopped get error = %v", err)
	}
	if _, err := service.ReadyAliases(context.Background(), request, "preview-1"); !errors.Is(err, ErrLeaseNotActive) {
		t.Fatalf("stopped aliases error = %v", err)
	}
}

func bytes32(value byte) []byte {
	key := make([]byte, 32)
	for index := range key {
		key[index] = value
	}
	return key
}

func deterministicIDs() func(prefix string) (string, error) {
	var n int
	return func(prefix string) (string, error) {
		n++
		return prefix + "-" + string(rune('a'+n)), nil
	}
}

func previewDomainRow(id, accountID, previewID, hostname, matchType string, previewGeneration int64, now time.Time) dbsqlc.PreviewDomain {
	return dbsqlc.PreviewDomain{ID: id, AccountID: accountID, PreviewID: previewID, PreviewGeneration: previewGeneration, Hostname: hostname, MatchType: matchType, OwnershipState: "verified", DnsTarget: "preview.example", ObservedRecords: []byte(`[]`), DnsProvider: "generic", ExpectedRecords: []byte(`[]`), DnsNextCheckAt: now, CertificateStrategy: "managed", CertificateReference: sql.NullString{String: "cert-ref", Valid: true}, CertificateState: "ready", CertificateExpiresAt: sql.NullTime{Time: now.Add(time.Hour), Valid: true}, CaaState: "ready", ConflictState: "clear", Generation: 2, CreatedAt: now, UpdatedAt: now}
}

type serviceRepositoryFake struct {
	lease  LeaseContext
	create CreateRecord
	list   []dbsqlc.PreviewDomain
	get    dbsqlc.PreviewDomain
	ready  []ReadyAliasRecord
}

func (f *serviceRepositoryFake) List(context.Context, string, string, *ListPosition, int) ([]dbsqlc.PreviewDomain, error) {
	return append([]dbsqlc.PreviewDomain(nil), f.list...), nil
}
func (f *serviceRepositoryFake) Get(context.Context, string, string, string) (dbsqlc.PreviewDomain, error) {
	if f.get.ID != "" {
		return f.get, nil
	}
	return previewDomainRow("pdom-a", "account-1", "preview-1", "alias.example", "exact", 4, time.Now().UTC()), nil
}
func (f *serviceRepositoryFake) Lease(context.Context, string, string) (LeaseContext, error) {
	return f.lease, nil
}
func (f *serviceRepositoryFake) Create(_ context.Context, input CreateRecord) (RepositoryMutation, error) {
	f.create = input
	row := previewDomainRow(input.DomainID, input.AccountID, input.PreviewID, input.Hostname, input.MatchType, input.PreviewGeneration, input.Now)
	return RepositoryMutation{Domain: row, Operation: dbsqlc.Operation{ID: input.OperationID, ResourceKind: Kind, ResourceID: sql.NullString{String: input.DomainID, Valid: true}, Phase: "waiting_for_dns", State: "running", Progress: 35, CorrelationID: input.CorrelationID, CreatedAt: input.Now, UpdatedAt: input.Now}, Changed: true}, nil
}
func (f *serviceRepositoryFake) Verify(context.Context, MutationRecord) (RepositoryMutation, error) {
	return RepositoryMutation{}, ErrInvalidInput
}
func (f *serviceRepositoryFake) Delete(context.Context, MutationRecord) (RepositoryMutation, error) {
	return RepositoryMutation{}, ErrInvalidInput
}
func (f *serviceRepositoryFake) ApplyDNSObservation(context.Context, DNSObservationRecord) (RepositoryMutation, error) {
	return RepositoryMutation{}, ErrInvalidInput
}
func (f *serviceRepositoryFake) ApplyCertificateObservation(context.Context, CertificateObservationRecord) (RepositoryMutation, error) {
	return RepositoryMutation{}, ErrInvalidInput
}
func (f *serviceRepositoryFake) ReadyAliases(context.Context, string, string, time.Time) ([]ReadyAliasRecord, error) {
	return append([]ReadyAliasRecord(nil), f.ready...), nil
}
