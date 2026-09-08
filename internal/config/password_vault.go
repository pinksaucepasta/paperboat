package config

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

const passwordVaultCustodyVersion = 1

// passwordVaultRecordBytes bounds the JSON representation before it is handed
// to encoding/json. The record can contain two envelopes and two plaintext key
// payloads, each at its contract limit, plus a small amount of metadata.
const passwordVaultRecordBytes = 2*((environmente2ee.MaximumVaultBytes+environmente2ee.MaximumScopeBytes)*4/3+4) + (2 << 20)

var ErrPasswordVaultInvalid = errors.New("ENV password vault custody record is invalid")

// PasswordVaultRecord is the local custody state for one issuer/account. The
// encrypted envelope and heads are retained while the optional Payload fields
// hold the locally unlocked key material. Pending is a crash-safe successor
// that remains available for publication reconciliation.
type PasswordVaultRecord struct {
	Head      environmente2ee.VaultHead `json:"head"`
	Envelope  []byte                    `json:"envelope,omitempty"`
	Payload   []byte                    `json:"payload,omitempty"`
	Pending   *PasswordVaultPending     `json:"pending,omitempty"`
	Operation *VaultOperation           `json:"operation,omitempty"`
}

// VaultOperation retains only an opaque encrypted mutation request. When Pending
// is also present, the service must commit that vault and operation atomically.
type VaultOperation struct {
	Kind      string          `json:"kind"`
	OwnerKind string          `json:"owner_kind,omitempty"`
	OwnerID   string          `json:"owner_id,omitempty"`
	MachineID string          `json:"machine_id,omitempty"`
	Request   json.RawMessage `json:"request"`
}

// PasswordVaultPending is the successor portion of PasswordVaultRecord.
type PasswordVaultPending struct {
	Head     environmente2ee.VaultHead `json:"head"`
	Envelope []byte                    `json:"envelope,omitempty"`
	Payload  []byte                    `json:"payload,omitempty"`
}

// Clear removes only unlocked key payloads. Envelopes, heads, and pending
// publication state remain available for a later unlock or reconciliation.
func (record *PasswordVaultRecord) Clear() {
	if record == nil {
		return
	}
	clear(record.Payload)
	record.Payload = nil
	if record.Pending != nil {
		clear(record.Pending.Payload)
		record.Pending.Payload = nil
	}
}

type passwordVaultCustodyRecord struct {
	Version   int                       `json:"version"`
	Head      environmente2ee.VaultHead `json:"head"`
	Envelope  []byte                    `json:"envelope,omitempty"`
	Payload   []byte                    `json:"payload,omitempty"`
	Pending   *PasswordVaultPending     `json:"pending,omitempty"`
	Operation *VaultOperation           `json:"operation,omitempty"`
}

func (record PasswordVaultRecord) custodyRecord() passwordVaultCustodyRecord {
	return passwordVaultCustodyRecord{
		Version:   passwordVaultCustodyVersion,
		Head:      record.Head,
		Envelope:  record.Envelope,
		Payload:   record.Payload,
		Pending:   record.Pending,
		Operation: record.Operation,
	}
}

func (record passwordVaultCustodyRecord) publicRecord() PasswordVaultRecord {
	return PasswordVaultRecord{
		Head:      record.Head,
		Envelope:  record.Envelope,
		Payload:   record.Payload,
		Pending:   record.Pending,
		Operation: record.Operation,
	}
}

func encodePasswordVaultRecord(record PasswordVaultRecord) ([]byte, error) {
	raw, err := json.Marshal(record.custodyRecord())
	if err != nil {
		return nil, err
	}
	if len(raw) > passwordVaultRecordBytes {
		clear(raw)
		return nil, ErrPasswordVaultInvalid
	}
	return raw, nil
}

func decodePasswordVaultRecord(encoded string) (PasswordVaultRecord, error) {
	if len(encoded) == 0 || len(encoded) > passwordVaultRecordBytes {
		return PasswordVaultRecord{}, ErrPasswordVaultInvalid
	}
	var stored passwordVaultCustodyRecord
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil || stored.Version != passwordVaultCustodyVersion {
		clearPasswordVaultCustodyPayloads(&stored)
		return PasswordVaultRecord{}, ErrPasswordVaultInvalid
	}
	canonical, err := json.Marshal(stored)
	if err != nil || !bytes.Equal(canonical, []byte(encoded)) {
		clear(canonical)
		clearPasswordVaultCustodyPayloads(&stored)
		return PasswordVaultRecord{}, ErrPasswordVaultInvalid
	}
	clear(canonical)
	record := stored.publicRecord()
	return record, nil
}

func clearPasswordVaultCustodyPayloads(record *passwordVaultCustodyRecord) {
	if record == nil {
		return
	}
	clear(record.Payload)
	if record.Pending != nil {
		clear(record.Pending.Payload)
	}
}

func passwordVaultSecretRef(issuer, accountID string) string {
	digest := sha256.Sum256([]byte(issuer + "\x00" + accountID + "\x00password_vault"))
	return "environment-password-vault-v1-" + hex.EncodeToString(digest[:16])
}

func normalizePasswordVaultCoordinates(s ProfileStore, issuer, accountID string) (string, string, error) {
	if err := s.RequireEnvironmentSecureStore(); err != nil {
		return "", "", err
	}
	normalized, err := NormalizeIssuer(issuer)
	if err != nil || !validCredentialID(accountID) {
		return "", "", ErrCredentialStoreUnavailable
	}
	return normalized, accountID, nil
}

func passwordVaultHeadIsZero(head environmente2ee.VaultHead) bool {
	return head.Issuer == "" && head.AccountID == "" && head.Generation == 0 && head.ID == (environmente2ee.DocumentID{})
}

func validatePasswordVaultPayload(payload, envelope []byte) error {
	if len(payload) == 0 {
		return nil
	}
	keys, err := environmente2ee.ParseVaultKeys(payload)
	if err != nil {
		return ErrPasswordVaultInvalid
	}
	defer keys.Clear()
	protection, err := environmente2ee.PasswordVaultProtection(envelope)
	if err != nil {
		return ErrPasswordVaultInvalid
	}
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(signer)
	if !bytes.Equal(signer.Public().(ed25519.PublicKey), protection.WriterPublic) {
		return ErrPasswordVaultInvalid
	}
	sharing, err := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
	if err != nil || !bytes.Equal(sharing.PublicKey().Bytes(), protection.SharingPublic) {
		return ErrPasswordVaultInvalid
	}
	return nil
}

func validatePasswordVaultEnvelope(head environmente2ee.VaultHead, envelope []byte, issuer, accountID string) (environmente2ee.DocumentID, error) {
	if head.Issuer != issuer || head.AccountID != accountID || head.Generation == 0 || head.Generation > environmente2ee.MaximumContractInteger ||
		head.ID == (environmente2ee.DocumentID{}) || len(envelope) == 0 || len(envelope) > environmente2ee.MaximumVaultBytes {
		return environmente2ee.DocumentID{}, ErrPasswordVaultInvalid
	}
	parsed, previous, err := environmente2ee.InspectPasswordVault(envelope)
	if err != nil || parsed != head || environmente2ee.DocumentID(sha256.Sum256(envelope)) != head.ID {
		return environmente2ee.DocumentID{}, ErrPasswordVaultInvalid
	}
	return previous, nil
}

func validatePasswordVaultRecord(record PasswordVaultRecord, issuer, accountID string) error {
	if op := record.Operation; op != nil {
		if len(op.Request) == 0 || len(op.Request) > 1<<20 || !json.Valid(op.Request) {
			return ErrPasswordVaultInvalid
		}
		switch op.Kind {
		case "scope-put", "team-create", "team-grant", "team-rotate", "reset", "host-provision", "personal-rotate":
		default:
			return ErrPasswordVaultInvalid
		}
	}
	currentEmpty := passwordVaultHeadIsZero(record.Head)
	if currentEmpty {
		if len(record.Envelope) != 0 || len(record.Payload) != 0 || record.Pending == nil {
			return ErrPasswordVaultInvalid
		}
	} else {
		if _, err := validatePasswordVaultEnvelope(record.Head, record.Envelope, issuer, accountID); err != nil {
			return err
		}
	}
	if err := validatePasswordVaultPayload(record.Payload, record.Envelope); err != nil {
		return err
	}
	if record.Pending == nil {
		return nil
	}
	pending := record.Pending
	previous, err := validatePasswordVaultEnvelope(pending.Head, pending.Envelope, issuer, accountID)
	if err != nil {
		return err
	}
	if currentEmpty {
		if pending.Head.Generation != 1 || previous != (environmente2ee.DocumentID{}) {
			return ErrPasswordVaultInvalid
		}
	} else {
		if record.Head.Generation == environmente2ee.MaximumContractInteger || pending.Head.Generation != record.Head.Generation+1 || pending.Head.ID == record.Head.ID || previous != record.Head.ID {
			return ErrPasswordVaultInvalid
		}
	}
	return validatePasswordVaultPayload(pending.Payload, pending.Envelope)
}

func normalizePasswordVaultRecord(record PasswordVaultRecord) (PasswordVaultRecord, string, string, error) {
	var issuer, accountID string
	if passwordVaultHeadIsZero(record.Head) {
		if record.Pending == nil {
			return PasswordVaultRecord{}, "", "", ErrPasswordVaultInvalid
		}
		issuer, accountID = record.Pending.Head.Issuer, record.Pending.Head.AccountID
	} else {
		issuer, accountID = record.Head.Issuer, record.Head.AccountID
	}
	normalized, err := NormalizeIssuer(issuer)
	if err != nil || !validCredentialID(accountID) {
		return PasswordVaultRecord{}, "", "", ErrPasswordVaultInvalid
	}
	if issuer != normalized {
		return PasswordVaultRecord{}, "", "", ErrPasswordVaultInvalid
	}
	if record.Pending != nil {
		if record.Pending.Head.Issuer != normalized || record.Pending.Head.AccountID != accountID {
			return PasswordVaultRecord{}, "", "", ErrPasswordVaultInvalid
		}
	}
	if err := validatePasswordVaultRecord(record, normalized, accountID); err != nil {
		return PasswordVaultRecord{}, "", "", err
	}
	return record, normalized, accountID, nil
}

func loadPasswordVaultRecord(store SecretStore, ref, issuer, accountID string) (PasswordVaultRecord, error) {
	encoded, err := store.Get(ref)
	if err != nil {
		return PasswordVaultRecord{}, err
	}
	record, err := decodePasswordVaultRecord(encoded)
	if err != nil {
		return PasswordVaultRecord{}, err
	}
	if err := validatePasswordVaultRecord(record, issuer, accountID); err != nil {
		record.Clear()
		return PasswordVaultRecord{}, err
	}
	return record, nil
}

func storePasswordVaultRecord(store SecretStore, ref string, record PasswordVaultRecord) error {
	raw, err := encodePasswordVaultRecord(record)
	if err != nil {
		return err
	}
	defer clear(raw)
	if err := store.Set(ref, string(raw)); err != nil {
		return fmt.Errorf("store ENV password vault custody: %w", err)
	}
	return nil
}

// LoadPasswordVault reads account-scoped custody from the OS secure store. It
// intentionally does not acquire the mutation lock: controller operations own
// LockEnvironmentMutations around their complete read/modify/publish flow.
func (s ProfileStore) LoadPasswordVault(issuer, accountID string) (PasswordVaultRecord, error) {
	normalized, accountID, err := normalizePasswordVaultCoordinates(s, issuer, accountID)
	if err != nil {
		return PasswordVaultRecord{}, err
	}
	return loadPasswordVaultRecord(s.Secrets, passwordVaultSecretRef(normalized, accountID), normalized, accountID)
}

// SavePasswordVault stores one complete account-scoped custody record. Callers
// that coordinate a remote publish must hold LockEnvironmentMutations with the
// password_vault subject before calling SavePasswordVault.
func (s ProfileStore) SavePasswordVault(record PasswordVaultRecord) error {
	if err := s.RequireEnvironmentSecureStore(); err != nil {
		return err
	}
	normalizedRecord, issuer, accountID, err := normalizePasswordVaultRecord(record)
	if err != nil {
		return err
	}
	return storePasswordVaultRecord(s.Secrets, passwordVaultSecretRef(issuer, accountID), normalizedRecord)
}

// LockPasswordVault clears unlocked payloads while retaining committed and
// pending envelopes/high-water state. This direct operation acquires the same
// account-scoped controller lock used by publish/reconciliation.
func (s ProfileStore) LockPasswordVault(issuer, accountID string) (resultErr error) {
	normalized, accountID, err := normalizePasswordVaultCoordinates(s, issuer, accountID)
	if err != nil {
		return err
	}
	unlock, err := s.LockEnvironmentMutations(normalized, accountID, "password_vault")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	record, err := loadPasswordVaultRecord(s.Secrets, passwordVaultSecretRef(normalized, accountID), normalized, accountID)
	if err != nil {
		return err
	}
	defer record.Clear()
	record.Clear()
	return storePasswordVaultRecord(s.Secrets, passwordVaultSecretRef(normalized, accountID), record)
}

// RemovePasswordVault is the explicit deletion boundary for password-vault
// custody. Session/logout cleanup deliberately does not call it.
func (s ProfileStore) RemovePasswordVault(issuer, accountID string) (resultErr error) {
	normalized, accountID, err := normalizePasswordVaultCoordinates(s, issuer, accountID)
	if err != nil {
		return err
	}
	unlock, err := s.LockEnvironmentMutations(normalized, accountID, "password_vault")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	if err := s.Secrets.Delete(passwordVaultSecretRef(normalized, accountID)); err != nil {
		return fmt.Errorf("remove ENV password vault custody: %w", err)
	}
	return nil
}
