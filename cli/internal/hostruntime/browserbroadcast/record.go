// Package browserbroadcast encodes the machine's shared browser-terminal output.
// It uses the RFC 9052 COSE_Encrypt0(COSE_Sign1) structures: the edge may copy
// these records but cannot read them or create a valid machine signature.
package browserbroadcast

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"

	"github.com/fxamacker/cbor/v2"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	MaxDataBytes        = 128 << 10
	MaxRecordBytes      = MaxDataBytes + 1024
	algEdDSA            = -8
	algChaCha20Poly1305 = 24
)

var ErrInvalidRecord = errors.New("invalid browser terminal broadcast record")

var (
	encodeMode       cbor.EncMode
	decodeMode       cbor.DecMode
	signProtected    []byte
	encryptProtected []byte
)

func init() {
	var err error
	encodeMode, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	decodeMode, err = (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 16, MaxArrayElements: 16, MaxMapPairs: 16}).DecMode()
	if err != nil {
		panic(err)
	}
	signProtected, err = encodeMode.Marshal(map[int]int{1: algEdDSA})
	if err != nil {
		panic(err)
	}
	encryptProtected, err = encodeMode.Marshal(map[int]int{1: algChaCha20Poly1305})
	if err != nil {
		panic(err)
	}
}

// Epoch has a fresh content key for one terminal output membership epoch.
// Key delivery occurs only inside each participant's authenticated inner TLS.
type Epoch struct {
	ID  [16]byte
	Key [chacha20poly1305.KeySize]byte
}

func NewEpoch() (Epoch, error) {
	var value Epoch
	if _, err := rand.Read(value.ID[:]); err != nil {
		return Epoch{}, err
	}
	if _, err := rand.Read(value.Key[:]); err != nil {
		return Epoch{}, err
	}
	return value, nil
}

// Record fields are signed by the machine and checked by every browser against
// its expected terminal, machine generation, epoch, and increasing record index.
type Record struct {
	SessionID     string
	Generation    uint64
	EpochID       [16]byte
	Index         uint64
	Channel       byte
	StartSequence uint64
	Data          []byte
}

func validRecord(value Record) bool {
	return value.SessionID != "" && len(value.SessionID) <= 128 && value.Generation != 0 && value.Index != 0 && (value.Channel == protocol.TerminalStdout || value.Channel == protocol.TerminalStderr) && len(value.Data) > 0 && len(value.Data) <= MaxDataBytes
}

// Seal uses a fresh deterministic nonce within an epoch. The caller must
// serialize Index and never reuse it with the same epoch key.
func Seal(value Record, epoch Epoch, private ed25519.PrivateKey) ([]byte, error) {
	if !validRecord(value) || value.EpochID != epoch.ID || len(private) != ed25519.PrivateKeySize {
		return nil, ErrInvalidRecord
	}
	// Reuse the established terminal-v1 adaptive zstd codec once at the machine;
	// all edge subscribers receive the same compressed ciphertext.
	output, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: value.Channel, StreamID: 1, StartSequence: value.StartSequence, Data: value.Data}, nil)
	if err != nil {
		return nil, err
	}
	payload, err := encodeMode.Marshal([]any{uint64(1), value.SessionID, value.Generation, value.EpochID[:], value.Index, output})
	if err != nil {
		return nil, err
	}
	toSign, err := encodeMode.Marshal([]any{"Signature1", signProtected, []byte{}, payload})
	if err != nil {
		return nil, err
	}
	signature := ed25519.Sign(private, toSign)
	signed, err := encodeMode.Marshal([]any{signProtected, map[int]any{}, payload, signature})
	if err != nil {
		return nil, err
	}
	var nonce [chacha20poly1305.NonceSize]byte
	copy(nonce[:4], epoch.ID[:4])
	binary.BigEndian.PutUint64(nonce[4:], value.Index)
	aead, err := chacha20poly1305.New(epoch.Key[:])
	if err != nil {
		return nil, err
	}
	aad, err := encodeMode.Marshal([]any{"Encrypt0", encryptProtected, []byte{}})
	if err != nil {
		return nil, err
	}
	ciphertext := aead.Seal(nil, nonce[:], signed, aad)
	result, err := encodeMode.Marshal([]any{encryptProtected, map[int]any{4: epoch.ID[:], 5: nonce[:]}, ciphertext})
	if err != nil || len(result) > MaxRecordBytes {
		return nil, ErrInvalidRecord
	}
	return result, nil
}

// Open authenticates both the AEAD and the machine-only signature. The caller
// must reject duplicate or skipped indexes using its last accepted index.
func Open(raw []byte, epoch Epoch, public ed25519.PublicKey) (Record, error) {
	if len(raw) == 0 || len(raw) > MaxRecordBytes || len(public) != ed25519.PublicKeySize {
		return Record{}, ErrInvalidRecord
	}
	var outer []cbor.RawMessage
	if err := decodeMode.Unmarshal(raw, &outer); err != nil || len(outer) != 3 {
		return Record{}, ErrInvalidRecord
	}
	var protected []byte
	var headers map[int][]byte
	var ciphertext []byte
	if decodeMode.Unmarshal(outer[0], &protected) != nil || !bytes.Equal(protected, encryptProtected) || decodeMode.Unmarshal(outer[1], &headers) != nil || len(headers) != 2 || decodeMode.Unmarshal(outer[2], &ciphertext) != nil {
		return Record{}, ErrInvalidRecord
	}
	if !bytes.Equal(headers[4], epoch.ID[:]) || len(headers[5]) != chacha20poly1305.NonceSize || len(ciphertext) < chacha20poly1305.Overhead {
		return Record{}, ErrInvalidRecord
	}
	aead, err := chacha20poly1305.New(epoch.Key[:])
	if err != nil {
		return Record{}, err
	}
	aad, _ := encodeMode.Marshal([]any{"Encrypt0", encryptProtected, []byte{}})
	signed, err := aead.Open(nil, headers[5], ciphertext, aad)
	if err != nil {
		return Record{}, ErrInvalidRecord
	}
	var sign1 []cbor.RawMessage
	if decodeMode.Unmarshal(signed, &sign1) != nil || len(sign1) != 4 {
		return Record{}, ErrInvalidRecord
	}
	var signHeader, payload, signature []byte
	var empty map[int]any
	if decodeMode.Unmarshal(sign1[0], &signHeader) != nil || !bytes.Equal(signHeader, signProtected) || decodeMode.Unmarshal(sign1[1], &empty) != nil || len(empty) != 0 || decodeMode.Unmarshal(sign1[2], &payload) != nil || decodeMode.Unmarshal(sign1[3], &signature) != nil || len(signature) != ed25519.SignatureSize {
		return Record{}, ErrInvalidRecord
	}
	toSign, _ := encodeMode.Marshal([]any{"Signature1", signProtected, []byte{}, payload})
	if !ed25519.Verify(public, toSign, signature) {
		return Record{}, ErrInvalidRecord
	}
	var fields []cbor.RawMessage
	if decodeMode.Unmarshal(payload, &fields) != nil || len(fields) != 6 {
		return Record{}, ErrInvalidRecord
	}
	var version uint64
	var record Record
	var epochID []byte
	var output []byte
	if decodeMode.Unmarshal(fields[0], &version) != nil || decodeMode.Unmarshal(fields[1], &record.SessionID) != nil || decodeMode.Unmarshal(fields[2], &record.Generation) != nil || decodeMode.Unmarshal(fields[3], &epochID) != nil || decodeMode.Unmarshal(fields[4], &record.Index) != nil || decodeMode.Unmarshal(fields[5], &output) != nil || version != 1 || len(epochID) != 16 {
		return Record{}, ErrInvalidRecord
	}
	copy(record.EpochID[:], epochID)
	frame, err := protocol.DecodeTerminalOutput(output)
	if err != nil || frame.StreamID != 1 {
		return Record{}, ErrInvalidRecord
	}
	record.Channel, record.StartSequence, record.Data = frame.Channel, frame.StartSequence, frame.Data
	var expectedNonce [chacha20poly1305.NonceSize]byte
	copy(expectedNonce[:4], epoch.ID[:4])
	binary.BigEndian.PutUint64(expectedNonce[4:], record.Index)
	if !validRecord(record) || record.EpochID != epoch.ID || !bytes.Equal(headers[5], expectedNonce[:]) {
		return Record{}, ErrInvalidRecord
	}
	return record, nil
}

// EpochHint identifies which authenticated key should be tried. This header
// is untrusted until Open verifies the ciphertext and signed inner record.
func EpochHint(raw []byte) ([16]byte, error) {
	var id [16]byte
	if len(raw) == 0 || len(raw) > MaxRecordBytes {
		return id, ErrInvalidRecord
	}
	var outer []cbor.RawMessage
	if decodeMode.Unmarshal(raw, &outer) != nil || len(outer) != 3 {
		return id, ErrInvalidRecord
	}
	var headers map[int][]byte
	if decodeMode.Unmarshal(outer[1], &headers) != nil || len(headers) != 2 || len(headers[4]) != 16 {
		return id, ErrInvalidRecord
	}
	copy(id[:], headers[4])
	return id, nil
}
