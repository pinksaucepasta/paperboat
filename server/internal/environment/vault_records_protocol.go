package environment

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

const MaximumVaultRecordBytes = 34943
const MaximumVaultRecordBatch = 128
const MaximumVaultRecordLiveBytes = 256 << 10
const MaximumVaultRecordHistoryBytes = 4 << 20
const MaximumVaultRecordIDs = 4096

var ErrVaultRecordCursorExpired = errors.New("encrypted ENV record cursor expired; fetch a new snapshot")

type VaultRecordState struct {
	VaultLayerCoordinate
	KeyEpoch     uint64 `json:"key_epoch"`
	RecordID     string `json:"record_id"`
	Revision     uint64 `json:"revision"`
	Deleted      bool   `json:"deleted"`
	Sequence     uint64 `json:"sequence"`
	DocumentID   string `json:"document_id"`
	Envelope     string `json:"envelope"`
	WriterPublic string `json:"writer_public"`
}
type VaultRecordMutation struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	Envelope         string `json:"envelope"`
}
type VaultRecordsPut struct {
	OperationID string                `json:"operation_id"`
	Records     []VaultRecordMutation `json:"records"`
}
type VaultRecordsPage struct {
	Records       []VaultRecordState `json:"records"`
	Sequence      uint64             `json:"sequence"`
	AfterSequence uint64             `json:"after_sequence"`
	NextCursor    string             `json:"next_cursor,omitempty"`
}
type VaultRecordsQuery struct {
	AfterSequence   uint64
	ThroughSequence *uint64
	Limit           int
	Cursor          string
}
type vaultRecordClaims struct {
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
type vaultRecordEnvelope struct {
	_          struct{} `cbor:",toarray"`
	Claims     []byte
	Ciphertext []byte
	Signature  []byte
}
type parsedVaultRecord struct {
	Claims          vaultRecordClaims
	Raw             []byte
	CiphertextBytes int
}

func validVaultRecordID(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == id
}
func parseVaultRecord(raw []byte, issuer string, writer PasswordVaultMetadata) (parsedVaultRecord, error) {
	var e vaultRecordEnvelope
	var c vaultRecordClaims
	if !decodeVaultCanonical(raw, MaximumVaultRecordBytes, &e) || !decodeVaultCanonical(e.Claims, 2048, &c) || c.Domain != "paperboat.environment.record" || c.Version != 1 || c.Issuer != issuer || !validLayerCoordinate(VaultLayerCoordinate{WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID}) || !validVaultRecordID(c.RecordID) || !validCounter(c.KeyEpoch) || !validCounter(c.Revision) || c.WriterAccount != writer.Head.AccountID || c.WriterVaultGeneration != writer.Head.Generation || len(c.Nonce) != 12 || len(e.Signature) != 64 || len(e.Ciphertext) <= 16 || len(e.Ciphertext) > 32927 || (c.Deleted && len(e.Ciphertext) > 160) {
		return parsedVaultRecord{}, ErrProtocolInvalid
	}
	message, err := EncodeCanonical([]any{"paperboat.environment.record-signature", uint64(1), e.Claims, e.Ciphertext})
	if err != nil || !ed25519.Verify(writer.WriterPublicKey[:], message, e.Signature) {
		return parsedVaultRecord{}, ErrProtocolSignature
	}
	return parsedVaultRecord{Claims: c, Raw: raw, CiphertextBytes: len(e.Ciphertext)}, nil
}
func recordState(c VaultLayerCoordinate, r parsedVaultRecord, sequence uint64, writer []byte) VaultRecordState {
	return VaultRecordState{VaultLayerCoordinate: c, KeyEpoch: r.Claims.KeyEpoch, RecordID: r.Claims.RecordID, Revision: r.Claims.Revision, Deleted: r.Claims.Deleted, Sequence: sequence, DocumentID: DocumentID(r.Raw), Envelope: base64.RawURLEncoding.EncodeToString(r.Raw), WriterPublic: base64.RawURLEncoding.EncodeToString(writer)}
}
