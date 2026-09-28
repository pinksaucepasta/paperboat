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
	if err := store.SavePeerDeviceSigningPublic(issuer, accountID, rootPublic); err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	document := api.EndpointCertificateDocument{Version: 1, AccountID: accountID, KeyID: "aek_" + hex.EncodeToString(rootFingerprint[:]), EndpointID: machineID, Role: "machine", Generation: 3, Serial: 2, IssuedAt: machine.Claims.IssuedAt.Format(time.RFC3339), ExpiresAt: machine.Claims.ExpiresAt.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(machineRaw), CertificateFingerprint: hex.EncodeToString(machineFingerprint[:])}
	authority, err := Resolve(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(_ context.Context, endpoint string, generation uint64) (api.EndpointCertificateDocument, error) {
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
	localOnly, localErr := ResolveLocal(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(context.Context, string, uint64) (api.EndpointCertificateDocument, error) {
		t.Fatal("local native authority fetched a machine certificate")
		return api.EndpointCertificateDocument{}, nil
	}, keys: testTransportKeys(rootPublic)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, Now: now})
	if localErr != nil || localOnly.LocalCertificate.Claims.EndpointID != cliID || len(localOnly.MachineCertificateRaw) != 0 {
		t.Fatalf("local native authority: %v", localErr)
	}
	localOnly.Clear()
	document.CertificateFingerprint = hex.EncodeToString(make([]byte, 32))
	if _, err := Resolve(context.Background(), Request{Store: store, Client: certificateClientFunc{fetch: func(context.Context, string, uint64) (api.EndpointCertificateDocument, error) { return document, nil }, keys: testTransportKeys(rootPublic)}, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, MachineID: machineID, MachineGeneration: 3, Now: now}); err == nil {
		t.Fatal("metadata substitution was accepted")
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
	if err := store.SavePeerDeviceSigningPublic(issuer, accountID, rootPublic); err != nil {
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
