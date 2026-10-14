package clientauthority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/trustedkeys"
)

var ErrInvalid = errors.New("peer client authority is invalid")

type CertificateClient interface {
	EndpointCertificate(context.Context, string, uint64) (api.EndpointCertificateDocument, error)
}

type transportKeyReader interface {
	PeerTransportKeys(context.Context) (api.PeerTransportKeySet, error)
}

type Request struct {
	Store              config.ProfileStore
	Client             CertificateClient
	Issuer             string
	AccountID          string
	CLIClientSessionID string
	MachineID          string
	MachineGeneration  uint64
	Now                time.Time
}

type Authority struct {
	// RootPublic is the local endpoint's signing key. It is retained only for
	// constructing this endpoint's QUIC leaf; peer certificates are verified
	// against TrustedKeys and never against this single key.
	RootPublic              ed25519.PublicKey
	TrustedKeys             []endpointidentity.TrustedKey
	LocalKeys               config.PeerIdentityKeys
	LocalCertificate        endpointidentity.Certificate
	LocalCertificateRaw     []byte
	MachineCertificate      endpointidentity.Certificate
	MachineCertificateKeyID string
	MachineCertificateRaw   []byte
}

func Resolve(ctx context.Context, request Request) (Authority, error) {
	result, err := resolve(ctx, request, false)
	return result, classifyResolutionFailure(ctx, err)
}

// ResolveLocal loads only the authenticated CLI identity. Native networking
// authenticates remote peers using server-signed key bindings, including
// inspector grants to another account's daemon.
func ResolveLocal(ctx context.Context, request Request) (Authority, error) {
	result, err := resolve(ctx, request, true)
	return result, classifyResolutionFailure(ctx, err)
}

func classifyResolutionFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		cause := context.Cause(ctx)
		contextErr := cause
		if cause == nil || !errors.Is(cause, ctx.Err()) {
			contextErr = errors.Join(ctx.Err(), cause)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return contextErr
		}
		return &resolutionFailure{err: errors.Join(err, contextErr)}
	}
	return &resolutionFailure{err: err}
}

func resolve(ctx context.Context, request Request, localOnly bool) (Authority, error) {
	if ctx == nil || request.Client == nil || request.AccountID == "" || request.CLIClientSessionID == "" || (!localOnly && (request.MachineID == "" || request.MachineGeneration == 0)) || request.Now.IsZero() {
		return Authority{}, ErrInvalid
	}
	// A long-lived CLI session renews its own certificate before expiry. The
	// authenticated session remains the authority; no other machine participates.
	if state, err := request.Store.LoadPeerCertificate(request.Issuer, request.CLIClientSessionID); err == nil {
		parsed, parseErr := endpointidentity.Parse(state.Raw)
		clear(state.Raw)
		if parseErr != nil {
			return Authority{}, ErrInvalid
		}
		if !request.Now.Add(identitybootstrap.CertificateRenewBefore).Before(parsed.Claims.ExpiresAt) {
			client, ok := request.Client.(identitybootstrap.CLIClient)
			if !ok {
				return Authority{}, ErrInvalid
			}
			if _, err := identitybootstrap.EnrollCLI(ctx, identitybootstrap.CLIRequest{Store: request.Store, Client: client, Issuer: request.Issuer, AccountID: request.AccountID, CLIClientSessionID: request.CLIClientSessionID, Now: func() time.Time { return request.Now }}); err != nil {
				return Authority{}, err
			}
		}
	} else if !errors.Is(err, config.ErrSecretNotFound) {
		return Authority{}, err
	}
	var keys config.PeerIdentityKeys
	var trusted []endpointidentity.TrustedKey
	var err error
	fail := func(err error) (Authority, error) {
		trustedkeys.Clear(trusted)
		clear(keys.RootPrivate)
		clear(keys.QUICPrivate)
		return Authority{}, err
	}
	reader, ok := request.Client.(transportKeyReader)
	if !ok {
		return fail(ErrInvalid)
	}
	root, rootErr := reader.PeerTransportKeys(ctx)
	if rootErr != nil {
		return fail(rootErr)
	}
	if root.Version != 1 {
		return fail(ErrInvalid)
	}
	trusted, err = trustedkeys.FromAPI(root.TrustedKeys)
	if err != nil {
		return fail(ErrInvalid)
	}
	keys, err = request.Store.PeerEndpointKeys(request.Issuer, request.AccountID, request.CLIClientSessionID)
	if err != nil {
		return fail(err)
	}
	localState, err := request.Store.LoadPeerCertificate(request.Issuer, request.CLIClientSessionID)
	if err != nil {
		return fail(err)
	}
	localDocument, err := request.Client.EndpointCertificate(ctx, request.CLIClientSessionID, 1)
	if err != nil {
		clear(localState.Raw)
		return fail(err)
	}
	localKey, ok := endpointidentity.TrustedKeyFor(trusted, localDocument.KeyID)
	if !ok {
		clear(localState.Raw)
		return fail(ErrInvalid)
	}
	documentRaw, decodeErr := base64.RawURLEncoding.Strict().DecodeString(localDocument.Certificate)
	localFingerprint := sha256.Sum256(localState.Raw)
	local, verifyErr := endpointidentity.VerifyWithTrustedKey(localState.Raw, localDocument.KeyID, trusted, endpointidentity.Expected{AccountID: request.AccountID, Role: endpointidentity.RoleCLI, EndpointID: request.CLIClientSessionID, Generation: 1}, request.Now.UTC())
	valid := decodeErr == nil && base64.RawURLEncoding.EncodeToString(documentRaw) == localDocument.Certificate && bytes.Equal(documentRaw, localState.Raw) && verifyErr == nil &&
		localDocument.Version == 1 && localDocument.AccountID == request.AccountID && localDocument.EndpointID == request.CLIClientSessionID && localDocument.Role == "cli" && localDocument.Generation == 1 &&
		localDocument.Serial == local.Claims.Serial && localDocument.IssuedAt == local.Claims.IssuedAt.Format(time.RFC3339) && localDocument.ExpiresAt == local.Claims.ExpiresAt.Format(time.RFC3339) && localDocument.CertificateFingerprint == hex.EncodeToString(localFingerprint[:]) &&
		bytes.Equal(local.Claims.QUICPublicKey, keys.QUICPrivate.Public().(ed25519.PublicKey))
	clear(documentRaw)
	if !valid {
		clear(localState.Raw)
		return fail(ErrInvalid)
	}
	if localOnly {
		localRootPublic := append(ed25519.PublicKey(nil), localKey.PublicKey...)
		return Authority{RootPublic: localRootPublic, TrustedKeys: trusted, LocalKeys: keys, LocalCertificate: local, LocalCertificateRaw: localState.Raw}, nil
	}
	document, err := request.Client.EndpointCertificate(ctx, request.MachineID, request.MachineGeneration)
	if err != nil {
		clear(localState.Raw)
		return fail(err)
	}
	machineKey, ok := endpointidentity.TrustedKeyFor(trusted, document.KeyID)
	if !ok {
		clear(localState.Raw)
		return fail(ErrInvalid)
	}
	machineRaw, decodeErr := base64.RawURLEncoding.Strict().DecodeString(document.Certificate)
	machineFingerprint := sha256.Sum256(machineRaw)
	machine, verifyErr := endpointidentity.Verify(machineRaw, machineKey.PublicKey, endpointidentity.Expected{AccountID: request.AccountID, Role: endpointidentity.RoleMachine, EndpointID: request.MachineID, Generation: request.MachineGeneration}, request.Now.UTC())
	if decodeErr != nil || base64.RawURLEncoding.EncodeToString(machineRaw) != document.Certificate || verifyErr != nil || document.Version != 1 || document.AccountID != request.AccountID || document.KeyID != machineKey.KeyID || document.EndpointID != request.MachineID || document.Role != "machine" || document.Generation != request.MachineGeneration || document.Serial != machine.Claims.Serial || document.IssuedAt != machine.Claims.IssuedAt.Format(time.RFC3339) || document.ExpiresAt != machine.Claims.ExpiresAt.Format(time.RFC3339) || document.CertificateFingerprint != hex.EncodeToString(machineFingerprint[:]) {
		clear(localState.Raw)
		clear(machineRaw)
		return fail(ErrInvalid)
	}
	localRootPublic := append(ed25519.PublicKey(nil), localKey.PublicKey...)
	return Authority{RootPublic: localRootPublic, TrustedKeys: trusted, LocalKeys: keys, LocalCertificate: local, LocalCertificateRaw: localState.Raw, MachineCertificate: machine, MachineCertificateKeyID: machineKey.KeyID, MachineCertificateRaw: machineRaw}, nil
}

func (a *Authority) Clear() {
	if a == nil {
		return
	}
	trustedkeys.Clear(a.TrustedKeys)
	clear(a.RootPublic)
	clear(a.LocalKeys.RootPrivate)
	clear(a.LocalKeys.QUICPrivate)
	clear(a.LocalCertificateRaw)
	clear(a.MachineCertificateRaw)
	*a = Authority{}
}
