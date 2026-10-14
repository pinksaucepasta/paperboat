package tunnelcert

import (
	"errors"
	"testing"
	"time"
)

func TestPlatformCertificateTargetDefinitionsAreDeterministic(t *testing.T) {
	definitions, err := PlatformCertificateTargetDefinitions(PlatformCertificateBases{
		PreviewBaseDomain: "Public.Example.test.",
		TunnelBaseDomain:  "public.example.test",
		RuntimeBaseDomain: "Runtime.Example.test.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 {
		t.Fatalf("platform definitions = %d, want 2", len(definitions))
	}
	want := []PlatformCertificateTargetDefinition{
		{ID: PlatformPublicTargetID, Kind: PlatformPublicWildcardTarget, Hostname: "*.public.example.test", AccountID: PlatformAccountID, ChallengeReference: PlatformPublicChallengeReference, Generation: 1},
		{ID: PlatformRuntimeTargetID, Kind: PlatformRuntimeWildcardTarget, Hostname: "*.runtime.example.test", AccountID: PlatformAccountID, ChallengeReference: PlatformRuntimeChallengeReference, Generation: 1},
	}
	for index := range want {
		if definitions[index] != want[index] {
			t.Fatalf("definition[%d] = %+v, want %+v", index, definitions[index], want[index])
		}
	}
	for _, bases := range []PlatformCertificateBases{
		{PreviewBaseDomain: "*.preview.example.test", TunnelBaseDomain: "tunnels.example.test"},
		{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "127.0.0.1"},
		{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "*.runtime.example.test"},
		{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test"},
	} {
		if _, err := PlatformCertificateTargetDefinitions(bases); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid platform bases %+v error = %v", bases, err)
		}
	}
}

func TestPlatformCertificateTargetGenerationFitsPostgresBigint(t *testing.T) {
	definition := PlatformCertificateTargetDefinition{
		ID: PlatformPublicTargetID, Kind: PlatformPublicWildcardTarget,
		Hostname: "*.preview.example.test", AccountID: PlatformAccountID,
		ChallengeReference: PlatformPublicChallengeReference, Generation: maxPlatformGeneration + 1,
	}
	if err := definition.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("generation above BIGINT error = %v", err)
	}
	definition.Generation = maxPlatformGeneration
	if err := definition.Validate(); err != nil {
		t.Fatalf("maximum BIGINT generation rejected: %v", err)
	}
}

func TestPlatformCertificateTargetDefinitionFencesBuiltInIdentity(t *testing.T) {
	definition := PlatformCertificateTargetDefinition{
		ID: PlatformPublicTargetID, Kind: PlatformPublicWildcardTarget,
		Hostname: "*.preview.example.test", AccountID: PlatformAccountID,
		ChallengeReference: PlatformPublicChallengeReference, Generation: 1,
	}
	for name, mutate := range map[string]func(*PlatformCertificateTargetDefinition){
		"account": func(value *PlatformCertificateTargetDefinition) { value.AccountID = "user-account" },
		"kind":    func(value *PlatformCertificateTargetDefinition) { value.Kind = PlatformRuntimeWildcardTarget },
		"id":      func(value *PlatformCertificateTargetDefinition) { value.ID = PlatformRuntimeTargetID },
		"challenge": func(value *PlatformCertificateTargetDefinition) {
			value.ChallengeReference = PlatformRuntimeChallengeReference
		},
	} {
		invalid := definition
		mutate(&invalid)
		if err := invalid.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("built-in %s identity error = %v", name, err)
		}
	}
}

func TestPlatformCertificateTargetDefinitionsRejectUnsharedOrRuntimeBases(t *testing.T) {
	for name, bases := range map[string]PlatformCertificateBases{
		"preview and tunnel": {
			PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnel.example.test", RuntimeBaseDomain: "runtime.example.test",
		},
		"preview and runtime": {
			PreviewBaseDomain: "shared.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "shared.example.test",
		},
		"tunnel and runtime": {
			PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "shared.example.test", RuntimeBaseDomain: "shared.example.test",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PlatformCertificateTargetDefinitions(bases); !errors.Is(err, ErrInvalid) {
				t.Fatalf("duplicate platform bases error = %v", err)
			}
		})
	}
}

func TestPlatformCertificateTargetValidationFencesRetryAndRevocationState(t *testing.T) {
	definition := PlatformCertificateTargetDefinition{
		ID: PlatformPublicTargetID, Kind: PlatformPublicWildcardTarget,
		Hostname: "*.preview.example.test", AccountID: PlatformAccountID,
		ChallengeReference: PlatformPublicChallengeReference, Generation: 1,
	}
	for name, mutate := range map[string]func(*PlatformCertificateTarget){
		"retry overflow":      func(value *PlatformCertificateTarget) { value.RetryCount = 31 },
		"revocation mismatch": func(value *PlatformCertificateTarget) { value.DesiredState = "revoked" },
	} {
		invalid := PlatformCertificateTarget{PlatformCertificateTargetDefinition: definition, DesiredState: "active", CertificateState: "pending"}
		mutate(&invalid)
		if err := invalid.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid %s error = %v", name, err)
		}
	}
}

func TestPlatformStoredCertificateRejectsPEMMaterial(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	value := StoredCertificate{
		ID: "tcert-platform-test", DomainID: PlatformPublicTargetID, AccountID: PlatformAccountID,
		TargetKind: TargetPlatformWildcard, Hostname: "*.preview.example.test", DomainGeneration: 1,
		CertificateGeneration: 1, Strategy: StrategyPlatformDNS01, State: StateStaged,
		CertificateReference: "tcert_platform_test", MasterKeyReference: "master/current",
		CertificateCiphertext: []byte("-----BEGIN CERTIFICATE----- plaintext material -----END CERTIFICATE-----"),
		PrivateKeyCiphertext:  []byte("encrypted-placeholder-with-more-than-29-bytes"),
		Fingerprint:           [32]byte{1}, Issuer: "test-ca", NotBefore: now,
		ExpiresAt: now.Add(time.Hour), RenewalAt: now.Add(30 * time.Minute), UpdatedAt: now,
	}
	if err := platformStoredCertificateValid(value); !errors.Is(err, ErrInvalid) {
		t.Fatalf("plaintext platform certificate error = %v", err)
	}
}

func TestPlatformDNSChallengeTargetWritesAuthorizationNameDirectly(t *testing.T) {
	domain := Domain{
		ID: PlatformPublicTargetID, AccountID: PlatformAccountID,
		TargetKind: TargetPlatformWildcard, Hostname: "*.Preview.Example.test",
	}
	if _, err := PlatformDNSChallengeTargetForDomain(domain); !errors.Is(err, ErrDNSChallengeUnavailable) {
		// Hostname canonicalization is an explicit storage contract. Keep this
		// check to prevent silently accepting a mixed-case platform projection.
		t.Fatalf("mixed-case platform hostname error = %v", err)
	}
	domain.Hostname = "*.preview.example.test"
	target, err := PlatformDNSChallengeTargetForDomain(domain)
	if err != nil {
		t.Fatal(err)
	}
	if target != "_acme-challenge.preview.example.test" {
		t.Fatalf("direct platform challenge target = %q", target)
	}
	delegated, err := DelegatedChallengeTargetForDomain(domain, "not-a-valid-zone")
	if err != nil {
		t.Fatal(err)
	}
	if delegated != target {
		t.Fatalf("platform challenge target through domain helper = %q, want %q", delegated, target)
	}
}

func TestPlatformWildcardDomainRejectsRouteAndLeaseIdentity(t *testing.T) {
	base := Domain{
		ID: PlatformRuntimeTargetID, AccountID: PlatformAccountID,
		TargetKind: TargetPlatformWildcard, Hostname: "*.tunnels.example.test",
		Generation: 1, Strategy: StrategyPlatformDNS01, OwnershipState: "verified",
		CAAState: "not_applicable",
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]Domain{
		"route":    func() Domain { value := base; value.RouteID = "route_platform"; return value }(),
		"tunnel":   func() Domain { value := base; value.TunnelID = "tunnel_platform"; return value }(),
		"lease":    func() Domain { value := base; value.PreviewID = "preview_platform"; return value }(),
		"strategy": func() Domain { value := base; value.Strategy = StrategyWildcard; return value }(),
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("platform %s identity error = %v", name, err)
		}
	}
	user := base
	user.TargetKind = TargetDurableRoute
	user.TunnelID = "tunnel_user"
	user.RouteID = "route_user"
	user.Strategy = StrategyPlatformDNS01
	if err := user.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("platform strategy on user target error = %v", err)
	}
}

func TestSharedPublicBaseHasOneCertificateOwner(t *testing.T) {
	definitions, err := PlatformCertificateTargetDefinitions(PlatformCertificateBases{
		PreviewBaseDomain: "public.example.test", TunnelBaseDomain: "public.example.test", RuntimeBaseDomain: "runtime.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 || definitions[0].Hostname != "*.public.example.test" || definitions[1].Hostname != "*.runtime.example.test" {
		t.Fatalf("shared public certificate owners = %+v", definitions)
	}
}
