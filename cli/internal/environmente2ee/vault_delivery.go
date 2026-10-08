package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/sha256"
)

type TeamGrantClaims struct {
	_                        struct{} `cbor:",toarray"`
	Domain                   string
	Version                  uint64
	Issuer                   string
	TeamID                   string
	TeamEpoch                uint64
	MembershipGeneration     uint64
	SenderAccount            string
	SenderWriterPublic       []byte
	RecipientAccount         string
	RecipientVaultGeneration uint64
	RecipientSharingPublic   []byte
	OperationID              string
}
type TeamGrant struct {
	Claims TeamGrantClaims
	ID     DocumentID
	Raw    []byte
}
type vaultDeliveryEnvelope struct {
	_          struct{} `cbor:",toarray"`
	Claims     []byte
	Enc        []byte
	Ciphertext []byte
	Signature  []byte
}

func deliverySignature(domain string, e vaultDeliveryEnvelope) []byte {
	raw, _ := encode([]any{domain + "-signature", uint64(1), e.Claims, e.Enc, e.Ciphertext})
	return raw
}
func sealVaultDelivery(ctx context.Context, domain string, claims, recipient, seed, plain []byte) ([]byte, error) {
	if ctx == nil || len(seed) != 32 || !validVaultPublic(recipient) {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	public, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(recipient)
	if err != nil {
		return nil, ErrInvalid
	}
	enc, sender, err := hpke.NewSender(public, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(domain+"/v1"))
	if err != nil {
		return nil, err
	}
	ciphertext, err := sender.Seal(claims, plain)
	if err != nil {
		return nil, err
	}
	e := vaultDeliveryEnvelope{Claims: claims, Enc: enc, Ciphertext: ciphertext}
	signer := ed25519.NewKeyFromSeed(seed)
	defer clear(signer)
	e.Signature = ed25519.Sign(signer, deliverySignature(domain, e))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return encode(e)
}
func parseVaultDelivery(raw, writer []byte, domain string, max int) (vaultDeliveryEnvelope, error) {
	var e vaultDeliveryEnvelope
	if len(writer) != 32 || decodeCanonical(raw, max, &e) != nil || len(e.Enc) != 32 || len(e.Signature) != 64 || len(e.Ciphertext) <= 16 || !ed25519.Verify(writer, deliverySignature(domain, e), e.Signature) {
		return e, ErrInvalid
	}
	return e, nil
}
func openVaultDelivery(ctx context.Context, domain string, e vaultDeliveryEnvelope, privateBytes []byte) ([]byte, error) {
	if ctx == nil || len(privateBytes) != 32 {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	private, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(privateBytes)
	if err != nil {
		return nil, ErrInvalid
	}
	recipient, err := hpke.NewRecipient(e.Enc, private, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(domain+"/v1"))
	if err != nil {
		return nil, ErrInvalid
	}
	plain, err := recipient.Open(e.Claims, e.Ciphertext)
	if err != nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		clear(plain)
		return nil, err
	}
	return plain, nil
}
func validTeamGrant(c TeamGrantClaims) bool {
	return c.Domain == "paperboat.environment.team-grant" && c.Version == 1 && validVaultIssuer(c.Issuer) && validIdentifier(c.TeamID) &&
		validVaultCounter(c.TeamEpoch) && validVaultCounter(c.MembershipGeneration) && validIdentifier(c.SenderAccount) && len(c.SenderWriterPublic) == 32 &&
		validIdentifier(c.RecipientAccount) && validVaultCounter(c.RecipientVaultGeneration) && validVaultPublic(c.RecipientSharingPublic) && validIdentifier(c.OperationID)
}
func SealTeamGrant(ctx context.Context, c TeamGrantClaims, key, writerSeed []byte) (TeamGrant, error) {
	if len(key) != 32 || len(writerSeed) != 32 {
		return TeamGrant{}, ErrInvalid
	}
	c.Domain = "paperboat.environment.team-grant"
	c.Version = 1
	signer := ed25519.NewKeyFromSeed(writerSeed)
	defer clear(signer)
	c.SenderWriterPublic = bytes.Clone(signer.Public().(ed25519.PublicKey))
	if !validTeamGrant(c) {
		return TeamGrant{}, ErrInvalid
	}
	claims, err := encode(c)
	if err != nil {
		return TeamGrant{}, err
	}
	raw, err := sealVaultDelivery(ctx, c.Domain, claims, c.RecipientSharingPublic, writerSeed, key)
	if err != nil {
		return TeamGrant{}, err
	}
	return TeamGrant{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: raw}, nil
}
func ParseTeamGrant(raw, authenticatedWriter []byte) (TeamGrant, error) {
	e, err := parseVaultDelivery(raw, authenticatedWriter, "paperboat.environment.team-grant", 4096)
	if err != nil || len(e.Ciphertext) != 48 {
		return TeamGrant{}, ErrInvalid
	}
	var c TeamGrantClaims
	if decodeCanonical(e.Claims, 3072, &c) != nil || !validTeamGrant(c) || !bytes.Equal(c.SenderWriterPublic, authenticatedWriter) {
		return TeamGrant{}, ErrInvalid
	}
	return TeamGrant{Claims: c, ID: DocumentID(sha256.Sum256(raw)), Raw: bytes.Clone(raw)}, nil
}
func OpenTeamGrant(ctx context.Context, g TeamGrant, issuer, account string, vaultGeneration, teamEpoch, membershipGeneration uint64, sharingPrivate []byte) ([]byte, error) {
	// Epoch and membership come from current authenticated team state. Possession
	// of a stale grant alone is never permission to consume a new membership.
	if g.Claims.Issuer != issuer || g.Claims.RecipientAccount != account || g.Claims.RecipientVaultGeneration > vaultGeneration || g.Claims.TeamEpoch != teamEpoch || g.Claims.MembershipGeneration != membershipGeneration || g.ID != DocumentID(sha256.Sum256(g.Raw)) {
		return nil, ErrInvalid
	}
	private, err := ecdh.X25519().NewPrivateKey(sharingPrivate)
	if err != nil || !bytes.Equal(private.PublicKey().Bytes(), g.Claims.RecipientSharingPublic) {
		return nil, ErrInvalid
	}
	e, err := parseVaultDelivery(g.Raw, g.Claims.SenderWriterPublic, g.Claims.Domain, 4096)
	if err != nil {
		return nil, err
	}
	claims, err := encode(g.Claims)
	if err != nil || !bytes.Equal(e.Claims, claims) {
		return nil, ErrInvalid
	}
	key, err := openVaultDelivery(ctx, g.Claims.Domain, e, sharingPrivate)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, ErrInvalid
	}
	return key, nil
}

// ProjectionSource pins the complete source head used to materialize selected
// values. The server fences projections whenever one of these heads changes.
