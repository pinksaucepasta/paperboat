package clientauthority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

type certificateClientFunc struct {
	fetch func(context.Context, string, uint64) (api.EndpointCertificateDocument, error)
	keys  api.PeerTransportKeySet
}

func (f certificateClientFunc) EndpointCertificate(ctx context.Context, endpoint string, generation uint64) (api.EndpointCertificateDocument, error) {
	return f.fetch(ctx, endpoint, generation)
}

func (f certificateClientFunc) PeerTransportKeys(context.Context) (api.PeerTransportKeySet, error) {
	return f.keys, nil
}

func testTransportKeys(public ed25519.PublicKey) api.PeerTransportKeySet {
	fingerprint := sha256.Sum256(public)
	return api.PeerTransportKeySet{Version: 1, TrustedKeys: []api.E2EEKey{{KeyID: "aek_" + hex.EncodeToString(fingerprint[:]), PublicKey: base64.RawURLEncoding.EncodeToString(public), Fingerprint: hex.EncodeToString(fingerprint[:]), Generation: 1}}}
}

func testCertificateDocument(t *testing.T, certificate endpointidentity.Certificate, public ed25519.PublicKey) api.EndpointCertificateDocument {
	t.Helper()
	raw, err := certificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(raw)
	key := sha256.Sum256(public)
	c := certificate.Claims
	return api.EndpointCertificateDocument{Version: 1, AccountID: c.AccountID, KeyID: "aek_" + hex.EncodeToString(key[:]), EndpointID: c.EndpointID, Role: "cli", Generation: c.Generation, Serial: c.Serial, IssuedAt: c.IssuedAt.Format(time.RFC3339), ExpiresAt: c.ExpiresAt.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: hex.EncodeToString(fingerprint[:])}
}
func TestResolveBindsLocalCustodyAndRemoteCertificateToOneRoot(t *testing.T) {
	root := t.TempDir()
	store := config.ProfileStore{Path: root, Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}
	issuer, accountID, cliID, machineID := "https://api.example.test", "account_01", "cli_01", "machine_01"
	keys, err := store.FreshPeerIdentityKeys(issuer, accountID, cliID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	local, err := endpointidentity.Sign(keys.RootPrivate, endpointidentity.Claims{AccountID: accountID, Role: endpointidentity.RoleCLI, EndpointID: cliID, QUICPublicKey: keys.QUICPrivate.Public().(ed25519.PublicKey), Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	localRaw, _ := local.MarshalBinary()
	if _, err := store.SavePeerCertificate(issuer, cliID, localRaw); err != nil {
		t.Fatal(err)
	}
	_, machineQUIC, _ := ed25519.GenerateKey(nil)
	machine, err := endpointidentity.Sign(keys.RootPrivate, endpointidentity.Claims{AccountID: accountID, Role: endpointidentity.RoleMachine, EndpointID: machineID, QUICPublicKey: machineQUIC.Public().(ed25519.PublicKey), Generation: 3, Serial: 2, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	machineRaw, _ := machine.MarshalBinary()
	machineFingerprint := sha256.Sum256(machineRaw)
	rootPublic := keys.RootPrivate.Public().(ed25519.PublicKey)
	if err := store.SavePeerMachineSigningPublic(issuer, accountID, rootPublic); err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	document := api.EndpointCertificateDocument{Version: 1, AccountID: accountID, KeyID: "aek_" + hex.EncodeToString(rootFingerprint[:]), EndpointID: machineID, Role: "machine", Generation: 3, Serial: 2, IssuedAt: machine.Claims.IssuedAt.Format(time.RFC3339), ExpiresAt: machine.Claims.ExpiresAt.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(machineRaw), CertificateFingerprint: hex.EncodeToString(machineFingerprint[:])}
	authority, err := Resolve(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(_ context.Context, endpoint string, generation uint64) (api.EndpointCertificateDocument, error) {
		if endpoint == cliID && generation == 1 {
			return testCertificateDocument(t, local, rootPublic), nil
		}
		if endpoint != machineID || generation != 3 {
			t.Fatalf("endpoint=%s generation=%d", endpoint, generation)
		}
		return document, nil
	}, keys: testTransportKeys(rootPublic)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, MachineID: machineID, MachineGeneration: 3, Now: now})
	if err != nil || authority.LocalCertificate.Claims.EndpointID != cliID || authority.MachineCertificate.Claims.EndpointID != machineID || len(authority.LocalKeys.QUICPrivate) != ed25519.PrivateKeySize {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	authority.Clear()
	if len(authority.RootPublic) != 0 || len(authority.LocalKeys.RootPrivate) != 0 || len(authority.MachineCertificateRaw) != 0 {
		t.Fatal("authority was not cleared")
	}
	// A pending enrollment writes another session's account-wide selector. The
	// active CLI must keep using its own authenticated certificate signer.
	otherSelector, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePeerMachineSigningPublic(issuer, accountID, otherSelector); err != nil {
		t.Fatal(err)
	}
	localOnly, localErr := ResolveLocal(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(_ context.Context, endpoint string, generation uint64) (api.EndpointCertificateDocument, error) {
		if endpoint != cliID || generation != 1 {
			t.Fatal("local authority fetched another endpoint")
		}
		return testCertificateDocument(t, local, rootPublic), nil
	}, keys: testTransportKeys(rootPublic)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, Now: now})
	if localErr != nil || localOnly.LocalCertificate.Claims.EndpointID != cliID || len(localOnly.MachineCertificateRaw) != 0 {
		t.Fatalf("local native authority: %v", localErr)
	}
	localOnly.Clear()
	for _, tc := range []struct {
		name   string
		mutate func(*api.EndpointCertificateDocument)
	}{
		{"version", func(d *api.EndpointCertificateDocument) { d.Version = 2 }},
		{"account", func(d *api.EndpointCertificateDocument) { d.AccountID = "account_other" }},
		{"session", func(d *api.EndpointCertificateDocument) { d.EndpointID = "cli_other" }},
		{"role", func(d *api.EndpointCertificateDocument) { d.Role = "machine" }},
		{"generation", func(d *api.EndpointCertificateDocument) { d.Generation = 2 }},
		{"serial", func(d *api.EndpointCertificateDocument) { d.Serial++ }},
		{"issued_at", func(d *api.EndpointCertificateDocument) { d.IssuedAt = now.Format(time.RFC3339) }},
		{"expires_at", func(d *api.EndpointCertificateDocument) { d.ExpiresAt = now.Format(time.RFC3339) }},
		{"fingerprint", func(d *api.EndpointCertificateDocument) {
			d.CertificateFingerprint = hex.EncodeToString(make([]byte, 32))
		}},
		{"missing_key", func(d *api.EndpointCertificateDocument) { d.KeyID = "" }},
		{"untrusted_key", func(d *api.EndpointCertificateDocument) { d.KeyID = "aek_other" }},
		{"different_raw", func(d *api.EndpointCertificateDocument) {
			d.Certificate = base64.RawURLEncoding.EncodeToString(machineRaw)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localDocument := testCertificateDocument(t, local, rootPublic)
			tc.mutate(&localDocument)
			_, err := ResolveLocal(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(context.Context, string, uint64) (api.EndpointCertificateDocument, error) {
				return localDocument, nil
			}, keys: testTransportKeys(rootPublic)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, Now: now})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid local metadata accepted: %v", err)
			}
		})
	}
	localDocument := testCertificateDocument(t, local, rootPublic)
	if _, err := ResolveLocal(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(context.Context, string, uint64) (api.EndpointCertificateDocument, error) {
		return localDocument, nil
	}, keys: testTransportKeys(otherSelector)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, Now: now}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked local signer accepted: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*api.EndpointCertificateDocument)
	}{
		{"remote_fingerprint", func(d *api.EndpointCertificateDocument) {
			d.CertificateFingerprint = hex.EncodeToString(make([]byte, 32))
		}},
		{"missing_remote_key", func(d *api.EndpointCertificateDocument) { d.KeyID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remoteDocument := document
			tc.mutate(&remoteDocument)
			remoteFetched := false
			_, err := Resolve(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(_ context.Context, endpoint string, generation uint64) (api.EndpointCertificateDocument, error) {
				if endpoint == cliID && generation == 1 {
					return localDocument, nil
				}
				if endpoint != machineID || generation != 3 {
					t.Fatalf("unexpected endpoint %s generation%d", endpoint, generation)
				}
				remoteFetched = true
				return remoteDocument, nil
			}, keys: testTransportKeys(rootPublic)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, MachineID: machineID, MachineGeneration: 3, Now: now})
			if !remoteFetched || !errors.Is(err, ErrInvalid) {
				t.Fatalf("remote metadata not rejected at intended boundary: fetched=%v err=%v", remoteFetched, err)
			}
		})
	}

}

func TestResolveFailurePreservesSentinelAndHasStaticAuthorityClassification(t *testing.T) {
	_, err := Resolve(context.Background(), Request{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("resolution error=%v", err)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(err, &staged) || staged.DiagnosticStage() != "peer_authority" ||
		!errors.As(err, &coded) || coded.DiagnosticCode() != "peer_authority_failed" {
		t.Fatalf("resolution classification missing: %T %v", err, err)
	}
	if got := err.Error(); got != "peer authority resolution failed" {
		t.Fatalf("resolution error text=%q", got)
	}
}

func TestClassifyResolutionFailurePreservesCustomCancellationCauseAndStatus(t *testing.T) {
	cause := errors.New("caller stopped peer authority resolution")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	err := classifyResolutionFailure(ctx, errors.New("private authority response"))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation status or cause lost: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	if errors.As(err, &staged) {
		t.Fatalf("canceled authority operation gained a diagnostic phase: %T %v", err, err)
	}
}

func TestClassifyResolutionDeadlineKeepsAuthorityPhaseAndCause(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cause := errors.New("private authority response")
	err := classifyResolutionFailure(ctx, cause)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("deadline or authority cause lost: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(err, &staged) || staged.DiagnosticStage() != "peer_authority" || !errors.As(err, &coded) || coded.DiagnosticCode() != "peer_authority_failed" {
		t.Fatalf("authority deadline classification=%T %v", err, err)
	}
}

func TestResolveUsesVerifierOnlyRootWithoutCreatingPrivateCustody(t *testing.T) {
	root := t.TempDir()
	store := config.ProfileStore{Path: root, Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}
	issuer, accountID, cliID, machineID := "https://api.example.test", "account_01", "cli_verifier", "machine_01"
	rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerMachineSigningPublic(issuer, accountID, rootPublic); err != nil {
		t.Fatal(err)
	}
	keys, err := store.PeerEndpointKeys(issuer, accountID, cliID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.RootPrivate) != 0 {
		t.Fatal("endpoint-only key creation loaded account root private custody")
	}

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	local, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{
		AccountID: accountID, Role: endpointidentity.RoleCLI, EndpointID: cliID,
		QUICPublicKey: keys.QUICPrivate.Public().(ed25519.PublicKey),
		Generation:    1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	localRaw, err := local.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePeerCertificate(issuer, cliID, localRaw); err != nil {
		t.Fatal(err)
	}

	machinePublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{
		AccountID: accountID, Role: endpointidentity.RoleMachine, EndpointID: machineID,
		QUICPublicKey: machinePublic,
		Generation:    3, Serial: 2, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	machineRaw, err := machine.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	machineFingerprint := sha256.Sum256(machineRaw)
	rootFingerprint := sha256.Sum256(rootPublic)
	document := api.EndpointCertificateDocument{
		Version: 1, AccountID: accountID, KeyID: "aek_" + hex.EncodeToString(rootFingerprint[:]),
		EndpointID: machineID, Role: "machine", Generation: 3, Serial: 2,
		IssuedAt: machine.Claims.IssuedAt.Format(time.RFC3339), ExpiresAt: machine.Claims.ExpiresAt.Format(time.RFC3339),
		Certificate: base64.RawURLEncoding.EncodeToString(machineRaw), CertificateFingerprint: hex.EncodeToString(machineFingerprint[:]),
	}
	request := Request{
		Store: store, Client: certificateClientFunc{fetch: func(_ context.Context, endpoint string, generation uint64) (api.EndpointCertificateDocument, error) {
			if endpoint == cliID && generation == 1 {
				return testCertificateDocument(t, local, rootPublic), nil
			}
			if endpoint != machineID || generation != 3 {
				t.Fatalf("endpoint=%q generation=%d", endpoint, generation)
			}
			return document, nil
		}, keys: testTransportKeys(rootPublic)},
		Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID,
		MachineID: machineID, MachineGeneration: 3, Now: now,
	}
	authority, err := Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(authority.RootPublic, rootPublic) || len(authority.LocalKeys.RootPrivate) != 0 ||
		!bytes.Equal(authority.LocalKeys.QUICPrivate, keys.QUICPrivate) ||
		authority.LocalCertificate.Claims.EndpointID != cliID || authority.MachineCertificate.Claims.EndpointID != machineID {
		t.Fatal("resolved verifier-only authority did not preserve the expected public root and endpoint custody")
	}
	if _, err := store.ExportPeerAccountRootSeed(issuer, accountID); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatalf("verifier-only resolution created root private custody: %v", err)
	}
	authority.Clear()

	otherPublic, otherPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	substituted, err := endpointidentity.Sign(otherPrivate, endpointidentity.Claims{
		AccountID: accountID, Role: endpointidentity.RoleMachine, EndpointID: machineID,
		QUICPublicKey: machinePublic,
		Generation:    3, Serial: 2, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	substitutedRaw, err := substituted.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	substitutedFingerprint := sha256.Sum256(substitutedRaw)
	otherRootFingerprint := sha256.Sum256(otherPublic)
	document.KeyID = "aek_" + hex.EncodeToString(otherRootFingerprint[:])
	document.Certificate = base64.RawURLEncoding.EncodeToString(substitutedRaw)
	document.CertificateFingerprint = hex.EncodeToString(substitutedFingerprint[:])
	if _, err := Resolve(context.Background(), request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("substituted verifier root error = %v", err)
	}
}
