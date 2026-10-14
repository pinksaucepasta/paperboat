package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

const MaximumVaultRecordBytes = MaximumValueBytes + MaximumNameBytes + 2048

// VaultRecordClaims binds one independently writable encrypted variable to its
// exact scope. Record identifiers reveal neither names nor values to storage.
type VaultRecordClaims struct {
	_                     struct{} `cbor:",toarray"`
	Domain                string
	Version               uint64
	Issuer                string
	WorkspaceID           string
	OwnerKind             string
	OwnerID               string
	MachineID             string
	KeyEpoch              uint64
	RecordID              string
	Revision              uint64
	Deleted               bool
	WriterAccount         string
	WriterVaultGeneration uint64
	Nonce                 []byte
}

type VaultRecord struct {
	Claims VaultRecordClaims
	ID     DocumentID
	Raw    []byte
}

func (r VaultRecord) CiphertextBytes() (int, error) {
	var e vaultRecordEnvelope
	if r.ID != DocumentID(sha256.Sum256(r.Raw)) || decodeCanonical(r.Raw, MaximumVaultRecordBytes, &e) != nil {
		return 0, ErrInvalid
	}
	return len(e.Ciphertext), nil
}

type vaultRecordEnvelope struct {
	_          struct{} `cbor:",toarray"`
	Claims     []byte
	Ciphertext []byte
	Signature  []byte
}
type vaultRecordValue struct {
	_     struct{} `cbor:",toarray"`
	Name  string
	Value []byte
}

// VaultRecordKey derives only the exact scope's key. Delivering this key never
// discloses another machine override, another workspace, or a vault root key.
func VaultRecordKey(root []byte, workspace, kind, owner, machine string, epoch uint64) ([]byte, error) {
	if len(root) != 32 || allZero(root) || !validIdentifier(workspace) || !validIdentifier(owner) || !validVaultCounter(epoch) ||
		(kind != "personal" && kind != "team") || (kind == "team" && (workspace != owner || machine != "")) ||
		(machine != "" && !validIdentifier(machine)) {
		return nil, ErrInvalid
	}
	binding, err := encode([]any{"paperboat.environment.record-key", uint64(1), workspace, kind, owner, machine, epoch})
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, root, nil, string(binding), 32)
}

func VaultRecordIdentifier(scopeKey []byte, name string) (string, error) {
	if len(scopeKey) != 32 || ValidateVariableName(name) != nil {
		return "", ErrInvalid
	}
	key, err := hkdf.Key(sha256.New, scopeKey, nil, "paperboat.environment.record-identifier/v1", 32)
	if err != nil {
		return "", err
	}
	defer clear(key)
	m := hmac.New(sha256.New, key)
	// Existing ENV policy treats case-folded names as one identity, including
	// Windows. A differently cased spelling must not create a second variable.
	_, _ = m.Write([]byte(strings.ToUpper(name)))
	return hex.EncodeToString(m.Sum(nil)), nil
}

func validRecordClaims(c VaultRecordClaims) bool {
	id, err := hex.DecodeString(c.RecordID)
	return c.Domain == "paperboat.environment.record" && c.Version == 1 && validVaultIssuer(c.Issuer) &&
		validIdentifier(c.WorkspaceID) && validIdentifier(c.OwnerID) &&
		(c.OwnerKind == "personal" || c.OwnerKind == "team" && c.WorkspaceID == c.OwnerID && c.MachineID == "") &&
		(c.MachineID == "" || c.OwnerKind == "personal" && validIdentifier(c.MachineID)) &&
		validVaultCounter(c.KeyEpoch) && err == nil && len(id) == 32 && hex.EncodeToString(id) == c.RecordID &&
		validVaultCounter(c.Revision) && validIdentifier(c.WriterAccount) &&
		(c.OwnerKind != "personal" || c.WriterAccount == c.OwnerID) && validVaultCounter(c.WriterVaultGeneration) && len(c.Nonce) == 12
}

func recordSignature(claims, ciphertext []byte) []byte {
	raw, _ := encode([]any{"paperboat.environment.record-signature", uint64(1), claims, ciphertext})
	return raw
}

func SealVaultRecord(ctx context.Context, c VaultRecordClaims, scopeKey, writerSeed []byte, name string, value []byte) (VaultRecord, error) {
	if ctx == nil || len(scopeKey) != 32 || len(writerSeed) != 32 || ValidateVariableName(name) != nil || len(value) > MaximumValueBytes || bytes.IndexByte(value, 0) >= 0 || !utf8.Valid(value) || (c.Deleted && len(value) != 0) {
		return VaultRecord{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return VaultRecord{}, err
	}
	id, err := VaultRecordIdentifier(scopeKey, name)
	if err != nil {
		return VaultRecord{}, err
	}
	if c.RecordID != "" && c.RecordID != id {
		return VaultRecord{}, ErrInvalid
	}
	c.Domain, c.Version, c.RecordID = "paperboat.environment.record", 1, id
	c.Nonce = make([]byte, 12)
	if _, err = rand.Read(c.Nonce); err != nil {
		return VaultRecord{}, err
	}
	if !validRecordClaims(c) {
		return VaultRecord{}, ErrInvalid
	}
	claims, err := encode(c)
	if err != nil {
		return VaultRecord{}, err
	}
	plain, err := encode(vaultRecordValue{Name: name, Value: value})
	if err != nil {
		return VaultRecord{}, err
	}
	defer clear(plain)
	aead, err := vaultAEAD(scopeKey)
	if err != nil {
		return VaultRecord{}, err
	}
	ciphertext := aead.Seal(nil, c.Nonce, plain, claims)
	signer := ed25519.NewKeyFromSeed(writerSeed)
	defer clear(signer)
	raw, err := encode(vaultRecordEnvelope{Claims: claims, Ciphertext: ciphertext, Signature: ed25519.Sign(signer, recordSignature(claims, ciphertext))})
	if err != nil {
		return VaultRecord{}, err
	}
	if err = ctx.Err(); err != nil {
		return VaultRecord{}, err
	}
	return VaultRecord{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: raw}, nil
}

func ParseVaultRecord(raw, writerPublic []byte) (VaultRecord, error) {
	var e vaultRecordEnvelope
	var c VaultRecordClaims
	if len(writerPublic) != ed25519.PublicKeySize || decodeCanonical(raw, MaximumVaultRecordBytes, &e) != nil || len(e.Claims) > 2048 ||
		len(e.Ciphertext) < 16 || len(e.Ciphertext) > MaximumValueBytes+MaximumNameBytes+64 || len(e.Signature) != 64 ||
		decodeCanonical(e.Claims, 2048, &c) != nil || !validRecordClaims(c) || !ed25519.Verify(writerPublic, recordSignature(e.Claims, e.Ciphertext), e.Signature) {
		return VaultRecord{}, ErrInvalid
	}
	return VaultRecord{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: bytes.Clone(raw)}, nil
}

func OpenVaultRecord(ctx context.Context, record VaultRecord, scopeKey []byte) (string, []byte, error) {
	if ctx == nil || len(scopeKey) != 32 || !validRecordClaims(record.Claims) || record.ID != DocumentID(sha256.Sum256(record.Raw)) {
		return "", nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	var e vaultRecordEnvelope
	if decodeCanonical(record.Raw, MaximumVaultRecordBytes, &e) != nil {
		return "", nil, ErrInvalid
	}
	claims, err := encode(record.Claims)
	if err != nil || !bytes.Equal(claims, e.Claims) {
		return "", nil, ErrInvalid
	}
	aead, err := vaultAEAD(scopeKey)
	if err != nil {
		return "", nil, err
	}
	plain, err := aead.Open(nil, record.Claims.Nonce, e.Ciphertext, e.Claims)
	if err != nil {
		return "", nil, ErrInvalid
	}
	defer clear(plain)
	var value vaultRecordValue
	if decodeCanonical(plain, MaximumValueBytes+MaximumNameBytes+64, &value) != nil || ValidateVariableName(value.Name) != nil || len(value.Value) > MaximumValueBytes || bytes.IndexByte(value.Value, 0) >= 0 || !utf8.Valid(value.Value) || record.Claims.Deleted && len(value.Value) != 0 {
		clear(value.Value)
		return "", nil, ErrInvalid
	}
	id, err := VaultRecordIdentifier(scopeKey, value.Name)
	if err != nil || id != record.Claims.RecordID {
		clear(value.Value)
		return "", nil, ErrInvalid
	}
	return value.Name, value.Value, nil
}
