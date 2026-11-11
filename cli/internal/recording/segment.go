// Package recording is the Paperboat terminal recording format: asciicast v2 event lines, compressed
// and encrypted in independent segment files. The host writes it, and the CLI and the browser (WASM)
// read it. Prior art: Tailscale sessionrecording (asciicast v2), Teleport recordingencryption (age to
// an X25519 key held away from nodes).
package recording

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/klauspost/compress/zstd"
)

// Magic starts every segment file.
const Magic = "PBREC1\n"

const (
	// MaxSegmentPlaintext bounds one segment's asciicast bytes; the host closes a segment well below it.
	MaxSegmentPlaintext = 4 << 20
	// MaxSegmentFile bounds one segment file a reader accepts.
	MaxSegmentFile = MaxSegmentPlaintext + 64<<10
	maxHeader      = 4 << 10
	dataKeyBytes   = 32
)

var ErrInvalid = errors.New("invalid recording segment")

// SealedKey is a recording's data key sealed with HPKE (X25519, HKDF-SHA256, AES-256-GCM) to the team
// recording public key of one generation.
type SealedKey struct {
	Encapsulated []byte `json:"enc"`
	Ciphertext   []byte `json:"ct"`
}

// SegmentHeader is authenticated as the AEAD's additional data. Every segment carries the sealed data
// key, so each one opens on its own.
type SegmentHeader struct {
	Recording      string    `json:"recording"`
	Index          uint32    `json:"index"`
	KeyGeneration  int64     `json:"key_generation"`
	SealedKey      SealedKey `json:"sealed_key"`
	PreviousSHA256 string    `json:"previous_sha256,omitempty"`
	Final          bool      `json:"final,omitempty"`
	EndReason      string    `json:"end_reason,omitempty"`
	PlaintextBytes int       `json:"plaintext_bytes"`
}

func NewDataKey() ([]byte, error) {
	key := make([]byte, dataKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

func dataKeyContext(recordingID string, generation int64) (info, aad []byte) {
	return []byte("paperboat recording data key v1"), []byte(recordingID + "\x00" + strconv.FormatInt(generation, 10))
}

func SealDataKey(teamPublic []byte, recordingID string, generation int64, dataKey []byte) (SealedKey, error) {
	if len(dataKey) != dataKeyBytes || recordingID == "" || generation < 1 {
		return SealedKey{}, ErrInvalid
	}
	public, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(teamPublic)
	if err != nil {
		return SealedKey{}, ErrInvalid
	}
	info, aad := dataKeyContext(recordingID, generation)
	enc, sender, err := hpke.NewSender(public, hpke.HKDFSHA256(), hpke.AES256GCM(), info)
	if err != nil {
		return SealedKey{}, ErrInvalid
	}
	ciphertext, err := sender.Seal(aad, dataKey)
	if err != nil {
		return SealedKey{}, ErrInvalid
	}
	return SealedKey{Encapsulated: enc, Ciphertext: ciphertext}, nil
}

func OpenDataKey(teamPrivate []byte, recordingID string, generation int64, sealed SealedKey) ([]byte, error) {
	private, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(teamPrivate)
	if err != nil {
		return nil, ErrInvalid
	}
	info, aad := dataKeyContext(recordingID, generation)
	recipient, err := hpke.NewRecipient(sealed.Encapsulated, private, hpke.HKDFSHA256(), hpke.AES256GCM(), info)
	if err != nil {
		return nil, ErrInvalid
	}
	key, err := recipient.Open(aad, sealed.Ciphertext)
	if err != nil || len(key) != dataKeyBytes {
		return nil, ErrInvalid
	}
	return key, nil
}

func segmentAEAD(dataKey []byte, index uint32) (cipher.AEAD, []byte, error) {
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, nil, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, ErrInvalid
	}
	// One data key per recording and one nonce per segment index: a nonce is never reused.
	nonce := make([]byte, aead.NonceSize())
	binary.BigEndian.PutUint32(nonce[len(nonce)-4:], index)
	return aead, nonce, nil
}

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	decoder, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxSegmentPlaintext), zstd.WithDecoderConcurrency(1))
)

// EncodeSegment returns the segment file: Magic, the header length (uint32 big endian), the header
// JSON, then AES-256-GCM(zstd(plaintext)).
func EncodeSegment(header SegmentHeader, dataKey, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxSegmentPlaintext || header.Recording == "" {
		return nil, ErrInvalid
	}
	header.PlaintextBytes = len(plaintext)
	encodedHeader, err := json.Marshal(header)
	if err != nil || len(encodedHeader) > maxHeader {
		return nil, ErrInvalid
	}
	aead, nonce, err := segmentAEAD(dataKey, header.Index)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(Magic)+4+len(encodedHeader)+len(plaintext)/2+aead.Overhead())
	out = append(out, Magic...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(encodedHeader)))
	out = append(out, encodedHeader...)
	return aead.Seal(out, nonce, encoder.EncodeAll(plaintext, nil), encodedHeader), nil
}

// ParseSegmentHeader reads a segment's header without the data key. The header is authenticated only
// once OpenSegment succeeds.
func ParseSegmentHeader(file []byte) (SegmentHeader, error) {
	header, _, _, err := splitSegment(file)
	return header, err
}

func splitSegment(file []byte) (SegmentHeader, []byte, []byte, error) {
	if len(file) > MaxSegmentFile || len(file) < len(Magic)+4 || string(file[:len(Magic)]) != Magic {
		return SegmentHeader{}, nil, nil, ErrInvalid
	}
	rest := file[len(Magic):]
	size := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	if size == 0 || size > maxHeader || int(size) > len(rest) {
		return SegmentHeader{}, nil, nil, ErrInvalid
	}
	encodedHeader := rest[:size]
	decoder := json.NewDecoder(bytes.NewReader(encodedHeader))
	decoder.DisallowUnknownFields()
	var header SegmentHeader
	if decoder.Decode(&header) != nil || decoder.More() || header.Recording == "" || header.KeyGeneration < 1 || header.PlaintextBytes < 0 || header.PlaintextBytes > MaxSegmentPlaintext {
		return SegmentHeader{}, nil, nil, ErrInvalid
	}
	return header, encodedHeader, rest[size:], nil
}

// OpenSegment authenticates and decrypts one segment file.
func OpenSegment(file, dataKey []byte) (SegmentHeader, []byte, error) {
	header, encodedHeader, ciphertext, err := splitSegment(file)
	if err != nil {
		return SegmentHeader{}, nil, err
	}
	aead, nonce, err := segmentAEAD(dataKey, header.Index)
	if err != nil {
		return SegmentHeader{}, nil, err
	}
	compressed, err := aead.Open(nil, nonce, ciphertext, encodedHeader)
	if err != nil {
		return SegmentHeader{}, nil, ErrInvalid
	}
	plaintext, err := decoder.DecodeAll(compressed, make([]byte, 0, header.PlaintextBytes))
	if err != nil || len(plaintext) != header.PlaintextBytes {
		return SegmentHeader{}, nil, ErrInvalid
	}
	return header, plaintext, nil
}
