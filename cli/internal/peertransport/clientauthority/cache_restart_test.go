package clientauthority

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
)

// paperboatAnswer is a control plane that hangs until the caller gives up (a dropped API port) or answers err.
type paperboatAnswer struct{ err error }

func (p paperboatAnswer) wait(ctx context.Context) error {
	if p.err != nil {
		return p.err
	}
	<-ctx.Done()
	return ctx.Err()
}
func (p paperboatAnswer) EndpointCertificate(ctx context.Context, _ string, _ uint64) (api.EndpointCertificateDocument, error) {
	return api.EndpointCertificateDocument{}, p.wait(ctx)
}
func (p paperboatAnswer) PeerTransportKeys(ctx context.Context) (api.PeerTransportKeySet, error) {
	return api.PeerTransportKeySet{}, p.wait(ctx)
}

// A daemon restarted while Paperboat is unreachable had no verified local identity in memory, so every new session
// waited for the server until its 10 s budget ran out ("could not reach the target machine", hp containers
// 2026-11-16). The identity last verified with Paperboat is now served from disk at once; a refusal the server
// answers in the background confirmation drops it, and a later restart does not serve it again.
func TestRestartWhilePaperboatIsUnreachableResolvesTheLastVerifiedLocalIdentity(t *testing.T) {
	root := t.TempDir()
	store := config.ProfileStore{Path: root, Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}
	issuer, accountID, cliID := "https://api.example.test", "account_01", "cli_01"
	keys, err := store.FreshPeerIdentityKeys(issuer, accountID, cliID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	local, err := endpointidentity.Sign(keys.RootPrivate, endpointidentity.Claims{AccountID: accountID, Role: endpointidentity.RoleCLI, EndpointID: cliID, QUICPublicKey: keys.QUICPrivate.Public().(ed25519.PublicKey), Generation: 1, Serial: 7, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	localRaw, _ := local.MarshalBinary()
	if _, err := store.SavePeerCertificate(issuer, cliID, localRaw); err != nil {
		t.Fatal(err)
	}
	rootPublic := keys.RootPrivate.Public().(ed25519.PublicKey)
	online := certificateClientFunc{keys: testTransportKeys(rootPublic), fetch: func(context.Context, string, uint64) (api.EndpointCertificateDocument, error) {
		return testCertificateDocument(t, local, rootPublic), nil
	}}
	request := func(client CertificateClient) Request {
		return Request{Store: store, Client: client, Issuer: issuer, AccountID: accountID, CLIClientSessionID: cliID, Now: now}
	}
	if _, err := NewCache().ResolveLocal(t.Context(), request(online)); err != nil {
		t.Fatal("online resolution:", err)
	}

	restarted := NewCache()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	got, err := restarted.ResolveLocal(ctx, request(paperboatAnswer{}))
	if err != nil || got.LocalCertificate.Claims.Serial != 7 || len(got.TrustedKeys) != 1 {
		t.Fatalf("restart with Paperboat unreachable: %+v, %v", got.LocalCertificate.Claims, err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("resolution waited %v for an unreachable Paperboat", waited)
	}

	refusing := NewCache()
	if _, err := refusing.ResolveLocal(t.Context(), request(paperboatAnswer{err: &api.APIError{Status: 403, Code: "endpoint_revoked"}})); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		refusing.mu.Lock()
		idle := len(refusing.refreshing) == 0
		refusing.mu.Unlock()
		if idle {
			break
		}
	}
	if _, err := NewCache().ResolveLocal(t.Context(), request(paperboatAnswer{err: &api.APIError{Status: 403}})); err == nil {
		t.Fatal("an identity Paperboat refused was served again after a restart")
	}
	if _, err := ResolveSavedLocal(request(nil)); !errors.Is(err, ErrNoSavedTrust) {
		t.Fatalf("saved trust after a refusal = %v, want none", err)
	}
}
