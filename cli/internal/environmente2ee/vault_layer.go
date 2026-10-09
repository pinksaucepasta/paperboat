package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
)

// VaultLayerSource is the exact source coordinate and authenticated encrypted head.
// Field order is the canonical v1 CBOR array contract shared with the control plane.
type VaultLayerSource struct {
	_           struct{} `cbor:",toarray"`
	WorkspaceID string
	OwnerKind   string
	OwnerID     string
	MachineID   string
	KeyEpoch    uint64
	Revision    uint64
	Digest      []byte
}
type VaultLayerClaims struct {
	_                      struct{} `cbor:",toarray"`
	Domain                 string
	Version                uint64
	Issuer                 string
	RecipientAccount       string
	MachineID              string
	InstallationGeneration uint64
	HostKeyGeneration      uint64
	HostPublic             []byte
	DeliveryGeneration     uint64
	Previous               []byte
	FenceGeneration        uint64
	Source                 VaultLayerSource
	WriterAccount          string
	WriterVaultGeneration  uint64
	WriterPublic           []byte
}
type VaultLayer struct {
	Claims VaultLayerClaims
	ID     DocumentID
	Raw    []byte
}

func validLayerSource(s VaultLayerSource) bool {
	return validIdentifier(s.WorkspaceID) && validIdentifier(s.OwnerID) &&
		((s.OwnerKind == "personal" && (s.MachineID == "" || validIdentifier(s.MachineID))) || (s.OwnerKind == "team" && s.WorkspaceID == s.OwnerID && s.WorkspaceID != "personal" && s.MachineID == "")) &&
		validVaultCounter(s.KeyEpoch) && validVaultCounter(s.Revision) && len(s.Digest) == 32 && !allZero(s.Digest)
}
func validLayer(c VaultLayerClaims) bool {
	return c.Domain == "paperboat.environment.host-layer" && c.Version == 1 && validVaultIssuer(c.Issuer) &&
		validIdentifier(c.RecipientAccount) && validIdentifier(c.MachineID) && validVaultCounter(c.InstallationGeneration) && validVaultCounter(c.HostKeyGeneration) && validVaultPublic(c.HostPublic) &&
		validVaultCounter(c.DeliveryGeneration) && len(c.Previous) == 32 && ((c.DeliveryGeneration == 1) == allZero(c.Previous)) && validVaultCounter(c.FenceGeneration) && validLayerSource(c.Source) &&
		(c.Source.MachineID == "" || c.Source.MachineID == c.MachineID) && validIdentifier(c.WriterAccount) && (c.Source.OwnerKind != "personal" || c.WriterAccount == c.Source.OwnerID && c.RecipientAccount == c.Source.OwnerID) &&
		validVaultCounter(c.WriterVaultGeneration) && validVaultPublic(c.WriterPublic)
}

// WorkspaceScopeKey isolates a member's private Team values from Personal and
// other Team workspaces. The caller owns and clears the returned copy.
func WorkspaceScopeKey(key []byte, workspace string) ([]byte, error) {
	if len(key) != 32 || !validIdentifier(workspace) {
		return nil, ErrInvalid
	}
	if workspace == "personal" {
		return bytes.Clone(key), nil
	}
	return hkdf.Key(sha256.New, key, nil, "paperboat.environment.workspace-key/v1\x00"+workspace, 32)
}
func SealVaultLayer(ctx context.Context, c VaultLayerClaims, writerSeed []byte, values map[string][]byte) (VaultLayer, error) {
	if ctx == nil || len(writerSeed) != 32 {
		return VaultLayer{}, ErrInvalid
	}
	c.Domain = "paperboat.environment.host-layer"
	c.Version = 1
	signer := ed25519.NewKeyFromSeed(writerSeed)
	defer clear(signer)
	c.WriterPublic = bytes.Clone(signer.Public().(ed25519.PublicKey))
	if !validLayer(c) {
		return VaultLayer{}, ErrInvalid
	}
	_, plain, err := encodeScope(values)
	if err != nil {
		return VaultLayer{}, err
	}
	defer clear(plain)
	claims, err := encode(c)
	if err != nil {
		return VaultLayer{}, err
	}
	raw, err := sealVaultDelivery(ctx, c.Domain, claims, c.HostPublic, writerSeed, plain)
	if err != nil {
		return VaultLayer{}, err
	}
	return VaultLayer{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: raw}, nil
}
func ParseVaultLayer(raw, writer []byte) (VaultLayer, error) {
	e, err := parseVaultDelivery(raw, writer, "paperboat.environment.host-layer", MaximumScopeBytes+65536)
	if err != nil || len(e.Ciphertext) > MaximumScopeBytes+16 {
		return VaultLayer{}, ErrInvalid
	}
	var c VaultLayerClaims
	if decodeCanonical(e.Claims, 65536, &c) != nil || !validLayer(c) || !bytes.Equal(c.WriterPublic, writer) {
		return VaultLayer{}, ErrInvalid
	}
	return VaultLayer{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: bytes.Clone(raw)}, nil
}
func OpenVaultLayer(ctx context.Context, p VaultLayer, expected VaultLayerClaims, privateBytes []byte) (map[string][]byte, error) {
	claims, err := encode(expected)
	actual, actualErr := encode(p.Claims)
	if ctx == nil || err != nil || actualErr != nil || !bytes.Equal(claims, actual) || p.ID != DocumentID(sha256.Sum256(p.Raw)) {
		return nil, ErrInvalid
	}
	private, err := ecdh.X25519().NewPrivateKey(privateBytes)
	if err != nil || !bytes.Equal(private.PublicKey().Bytes(), p.Claims.HostPublic) {
		return nil, ErrInvalid
	}
	e, err := parseVaultDelivery(p.Raw, expected.WriterPublic, "paperboat.environment.host-layer", MaximumScopeBytes+65536)
	if err != nil || !bytes.Equal(e.Claims, claims) {
		return nil, ErrInvalid
	}
	plain, err := openVaultDelivery(ctx, p.Claims.Domain, e, privateBytes)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	values, _, err := decodeScope(plain)
	return values, err
}

// SealVaultScopeKey grants an exact scope key once per recipient/key epoch.
// Ordinary variable edits do not invalidate or republish this grant.
func SealVaultScopeKey(ctx context.Context, c VaultLayerClaims, writerSeed, scopeKey []byte) (VaultLayer, error) {
	if ctx == nil || len(writerSeed) != 32 || len(scopeKey) != 32 || allZero(scopeKey) {
		return VaultLayer{}, ErrInvalid
	}
	c.Domain, c.Version = "paperboat.environment.host-layer", 1
	signer := ed25519.NewKeyFromSeed(writerSeed)
	defer clear(signer)
	c.WriterPublic = bytes.Clone(signer.Public().(ed25519.PublicKey))
	if !validLayer(c) {
		return VaultLayer{}, ErrInvalid
	}
	claims, err := encode(c)
	if err != nil {
		return VaultLayer{}, err
	}
	raw, err := sealVaultDelivery(ctx, c.Domain, claims, c.HostPublic, writerSeed, scopeKey)
	if err != nil {
		return VaultLayer{}, err
	}
	return VaultLayer{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: raw}, nil
}

func OpenVaultScopeKey(ctx context.Context, p VaultLayer, expected VaultLayerClaims, privateBytes []byte) ([]byte, error) {
	claims, err := encode(expected)
	actual, actualErr := encode(p.Claims)
	if ctx == nil || err != nil || actualErr != nil || !bytes.Equal(claims, actual) || !validLayer(p.Claims) || p.ID != DocumentID(sha256.Sum256(p.Raw)) {
		return nil, ErrInvalid
	}
	private, err := ecdh.X25519().NewPrivateKey(privateBytes)
	if err != nil || !bytes.Equal(private.PublicKey().Bytes(), p.Claims.HostPublic) {
		return nil, ErrInvalid
	}
	e, err := parseVaultDelivery(p.Raw, expected.WriterPublic, "paperboat.environment.host-layer", 4096)
	if err != nil || !bytes.Equal(e.Claims, claims) {
		return nil, ErrInvalid
	}
	key, err := openVaultDelivery(ctx, p.Claims.Domain, e, privateBytes)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 || allZero(key) {
		clear(key)
		return nil, ErrInvalid
	}
	return key, nil
}
