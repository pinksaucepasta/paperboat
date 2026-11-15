package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/hex"
)

const storedFileKeyDomain = "paperboat.files.stored-key"
const maximumStoredFileKeyBytes = 2048

// StoredFileClaims binds a file key to its immutable manifest and one of the
// recipient's existing ENV machine keys. It does not require a vault unlock.
type StoredFileClaims struct {
	_              struct{} `cbor:",toarray"`
	Domain         string
	Version        uint64
	RequestID      string
	ManifestDigest string
	MachineID      string
	HostPublic     []byte
	WriterPublic   []byte
}

func validStoredFileClaims(c StoredFileClaims) bool {
	digest, err := hex.DecodeString(c.ManifestDigest)
	return c.Domain == storedFileKeyDomain && c.Version == 1 &&
		validIdentifier(c.RequestID) && validIdentifier(c.MachineID) &&
		err == nil && len(digest) == 32 && hex.EncodeToString(digest) == c.ManifestDigest &&
		validVaultPublic(c.HostPublic) && len(c.WriterPublic) == ed25519.PublicKeySize
}

// SealStoredFileKey seals a per-transfer key with the same HPKE suite as ENV
// deliveries. sign is the source machine identity's signing method; its private
// key stays with the identity owner. WriterPublic must be that identity's key.
func SealStoredFileKey(ctx context.Context, c StoredFileClaims, key []byte, sign func([]byte) []byte) ([]byte, error) {
	c.Domain, c.Version = storedFileKeyDomain, 1
	if len(key) != 32 || !validStoredFileClaims(c) {
		return nil, ErrInvalid
	}
	claims, err := encode(c)
	if err != nil {
		return nil, err
	}
	raw, err := sealVaultDeliveryWithSigner(ctx, c.Domain, claims, c.HostPublic, key, sign)
	if err != nil {
		return nil, err
	}
	// A mismatched or broken signer must fail before the sender uploads a file
	// whose key none of its recipients could authenticate.
	if _, err := parseVaultDelivery(raw, c.WriterPublic, c.Domain, maximumStoredFileKeyBytes); err != nil {
		return nil, err
	}
	return raw, nil
}

// OpenStoredFileKey authenticates the sender and exact request before opening
// the envelope with the recipient machine's ENV private key. authenticatedWriter
// comes from the authenticated source identity, never from this envelope.
func OpenStoredFileKey(ctx context.Context, raw, authenticatedWriter, hostPrivate []byte, requestID, digest, machineID string) ([]byte, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, err := parseVaultDelivery(raw, authenticatedWriter, storedFileKeyDomain, maximumStoredFileKeyBytes)
	if err != nil || len(e.Ciphertext) != 48 {
		return nil, ErrInvalid
	}
	var c StoredFileClaims
	if decodeCanonical(e.Claims, maximumStoredFileKeyBytes, &c) != nil || !validStoredFileClaims(c) ||
		c.RequestID != requestID || c.ManifestDigest != digest || c.MachineID != machineID ||
		!bytes.Equal(c.WriterPublic, authenticatedWriter) {
		return nil, ErrInvalid
	}
	private, err := ecdh.X25519().NewPrivateKey(hostPrivate)
	if err != nil || !bytes.Equal(private.PublicKey().Bytes(), c.HostPublic) {
		return nil, ErrInvalid
	}
	key, err := openVaultDelivery(ctx, storedFileKeyDomain, e, hostPrivate)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		clear(key)
		return nil, ErrInvalid
	}
	return key, nil
}
