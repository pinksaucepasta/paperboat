package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"unicode/utf8"
)

const MaximumVaultScopeBytes = MaximumScopeBytes + 2048

// VaultScopeClaims identifies the complete personal/team value set. Machine
// overrides use the personal key; no scope key is ever delivered to a host.
type VaultScopeClaims struct {
	_                     struct{} `cbor:",toarray"`
	Domain                string
	Version               uint64
	Issuer                string
	OwnerKind             string
	OwnerID               string
	MachineID             string
	KeyEpoch              uint64
	Revision              uint64
	Previous              []byte
	WriterAccount         string
	WriterVaultGeneration uint64
	Nonce                 []byte
}
type VaultScope struct {
	Claims VaultScopeClaims
	ID     DocumentID
	Raw    []byte
}
type vaultScopeEnvelope struct {
	_          struct{} `cbor:",toarray"`
	Header     []byte
	Ciphertext []byte
	Signature  []byte
}

func validVaultCounter(n uint64) bool { return n > 0 && n <= MaximumContractInteger }
func validVaultIssuer(s string) bool  { return len(s) > 0 && len(s) <= 1024 && utf8.ValidString(s) }
func validScopeClaims(c VaultScopeClaims) bool {
	return c.Domain == "paperboat.environment.vault-scope" && c.Version == 1 && validVaultIssuer(c.Issuer) &&
		(c.OwnerKind == "personal" || c.OwnerKind == "team") && validIdentifier(c.OwnerID) &&
		(c.MachineID == "" || c.OwnerKind == "personal" && validIdentifier(c.MachineID)) &&
		validVaultCounter(c.KeyEpoch) && validVaultCounter(c.Revision) && len(c.Previous) == 32 &&
		((c.Revision == 1) == bytes.Equal(c.Previous, make([]byte, 32))) && validIdentifier(c.WriterAccount) &&
		(c.OwnerKind != "personal" || c.WriterAccount == c.OwnerID) && validVaultCounter(c.WriterVaultGeneration) && len(c.Nonce) == 12
}
func vaultScopeSignature(header, ciphertext []byte) []byte {
	raw, _ := encode([]any{"paperboat.environment.vault-scope-signature", uint64(1), header, ciphertext})
	return raw
}
func SealVaultScope(ctx context.Context, c VaultScopeClaims, key, writerSeed []byte, values map[string][]byte) (VaultScope, error) {
	if ctx == nil || len(key) != 32 || len(writerSeed) != 32 {
		return VaultScope{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return VaultScope{}, err
	}
	c.Domain = "paperboat.environment.vault-scope"
	c.Version = 1
	c.Nonce = make([]byte, 12)
	if _, err := rand.Read(c.Nonce); err != nil {
		return VaultScope{}, err
	}
	if !validScopeClaims(c) {
		return VaultScope{}, ErrInvalid
	}
	_, plain, err := encodeScope(values)
	if err != nil {
		return VaultScope{}, err
	}
	defer clear(plain)
	header, err := encode(c)
	if err != nil {
		return VaultScope{}, err
	}
	aead, err := vaultAEAD(key)
	if err != nil {
		return VaultScope{}, err
	}
	ciphertext := aead.Seal(nil, c.Nonce, plain, header)
	signer := ed25519.NewKeyFromSeed(writerSeed)
	defer clear(signer)
	raw, err := encode(vaultScopeEnvelope{Header: header, Ciphertext: ciphertext, Signature: ed25519.Sign(signer, vaultScopeSignature(header, ciphertext))})
	if err != nil {
		return VaultScope{}, err
	}
	if err = ctx.Err(); err != nil {
		return VaultScope{}, err
	}
	return VaultScope{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: raw}, nil
}

// ParseVaultScope requires an authenticated writer binding supplied by the
// current authorized scope response, never a public key chosen by ciphertext.
func ParseVaultScope(raw, writerPublic []byte) (VaultScope, error) {
	var e vaultScopeEnvelope
	if len(writerPublic) != 32 || decodeCanonical(raw, MaximumVaultScopeBytes, &e) != nil || len(e.Header) > 2048 || len(e.Ciphertext) <= 16 || len(e.Ciphertext) > MaximumScopeBytes+16 || len(e.Signature) != 64 {
		return VaultScope{}, ErrInvalid
	}
	var c VaultScopeClaims
	if decodeCanonical(e.Header, 2048, &c) != nil || !validScopeClaims(c) || !ed25519.Verify(writerPublic, vaultScopeSignature(e.Header, e.Ciphertext), e.Signature) {
		return VaultScope{}, ErrInvalid
	}
	return VaultScope{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: bytes.Clone(raw)}, nil
}
func OpenVaultScope(ctx context.Context, scope VaultScope, key []byte) (map[string][]byte, error) {
	if ctx == nil || len(key) != 32 || !validScopeClaims(scope.Claims) || scope.ID != DocumentID(sha256.Sum256(scope.Raw)) {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var e vaultScopeEnvelope
	if decodeCanonical(scope.Raw, MaximumVaultScopeBytes, &e) != nil {
		return nil, ErrInvalid
	}
	expected, err := encode(scope.Claims)
	if err != nil || !bytes.Equal(expected, e.Header) {
		return nil, ErrInvalid
	}
	aead, err := vaultAEAD(key)
	if err != nil {
		return nil, ErrInvalid
	}
	plain, err := aead.Open(nil, scope.Claims.Nonce, e.Ciphertext, e.Header)
	if err != nil {
		return nil, ErrInvalid
	}
	defer clear(plain)
	values, _, err := decodeScope(plain)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		clearValues(values)
		return nil, err
	}
	return values, nil
}
