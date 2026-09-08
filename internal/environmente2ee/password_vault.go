package environmente2ee

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"unicode/utf8"
)

const MaximumVaultBytes = MaximumScopeBytes + 2048

var ErrVaultUnlock = errors.New("ENV vault unlock failed")
var vaultKDFSlot = make(chan struct{}, 1)

type VaultHead struct {
	Issuer     string
	AccountID  string
	Generation uint64
	ID         DocumentID
}
type VaultCredential struct {
	_               struct{} `cbor:",toarray"`
	Kind            uint64
	Epoch           uint64
	Profile         uint64
	Salt            []byte
	RecipientPublic []byte
	SigningPublic   []byte
	Delegation      []byte
}
type VaultProtection struct {
	WriterPublic  []byte
	SharingPublic []byte
	Password      VaultCredential
	RecoveryEpoch uint64
	Recovery      *VaultCredential
}
type vaultHeader struct {
	_             struct{} `cbor:",toarray"`
	Domain        string
	Version       uint64
	Issuer        string
	AccountID     string
	Generation    uint64
	Previous      []byte
	WriterPublic  []byte
	Password      VaultCredential
	RecoveryEpoch uint64
	Recovery      *VaultCredential
	Nonce         []byte
	SharingPublic []byte
}
type vaultBody struct {
	_            struct{} `cbor:",toarray"`
	Header       []byte
	PasswordWrap []byte
	RecoveryWrap []byte
	Ciphertext   []byte
}
type vaultEnvelope struct {
	_         struct{} `cbor:",toarray"`
	Body      []byte
	Signature []byte
}

func credentialContext(head VaultHead, d VaultCredential) []byte {
	raw, _ := encode([]any{"paperboat.environment.vault-credential", uint64(1), head.Issuer, head.AccountID, d.Kind, d.Epoch, d.Profile, d.Salt})
	return raw
}
func delegationMessage(head VaultHead, d VaultCredential, writer []byte) []byte {
	raw, _ := encode([]any{"paperboat.environment.vault-delegation", uint64(1), credentialContext(head, d), d.RecipientPublic, d.SigningPublic, writer})
	return raw
}
func vaultSignatureMessage(body []byte) []byte {
	raw, _ := encode([]any{"paperboat.environment.vault-signature", uint64(1), body})
	return raw
}
func validVaultPublic(public []byte) bool {
	if len(public) != 32 || allZero(public) {
		return false
	}
	// Reject low-order X25519 recipients before any production wrap is attempted.
	private, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		return false
	}
	key, err := ecdh.X25519().NewPublicKey(public)
	if err != nil {
		return false
	}
	shared, err := private.ECDH(key)
	clear(shared)
	return err == nil
}
func (d VaultCredential) valid(head VaultHead, writer []byte, kind uint64) bool {
	return d.Kind == kind && d.Epoch > 0 && d.Epoch <= MaximumContractInteger &&
		((kind == 1 && d.Profile == 1 && len(d.Salt) == 16) || (kind == 2 && d.Profile == 0 && len(d.Salt) == 0)) &&
		validVaultPublic(d.RecipientPublic) && len(d.SigningPublic) == 32 && len(d.Delegation) == 64 &&
		ed25519.Verify(d.SigningPublic, delegationMessage(head, d, writer), d.Delegation)
}
func (h vaultHeader) head() VaultHead {
	return VaultHead{Issuer: h.Issuer, AccountID: h.AccountID, Generation: h.Generation}
}
func (h vaultHeader) valid() bool {
	return h.Domain == "paperboat.environment.password-vault" && h.Version == 1 &&
		len(h.Issuer) > 0 && len(h.Issuer) <= 1024 && utf8.ValidString(h.Issuer) && validIdentifier(h.AccountID) &&
		h.Generation > 0 && h.Generation <= MaximumContractInteger && len(h.Previous) == 32 && (h.Generation == 1) == allZero(h.Previous) &&
		len(h.WriterPublic) == 32 && !allZero(h.WriterPublic) && h.Password.valid(h.head(), h.WriterPublic, 1) &&
		h.RecoveryEpoch > 0 && h.RecoveryEpoch <= MaximumContractInteger &&
		(h.Recovery == nil || h.Recovery.Epoch == h.RecoveryEpoch && h.Recovery.valid(h.head(), h.WriterPublic, 2)) && len(h.Nonce) == 12 && validVaultPublic(h.SharingPublic)
}
func parseVault(raw []byte) (vaultEnvelope, vaultBody, vaultHeader, error) {
	var e vaultEnvelope
	var b vaultBody
	var h vaultHeader
	if decodeCanonical(raw, MaximumVaultBytes, &e) != nil || len(e.Signature) != 64 || decodeCanonical(e.Body, MaximumVaultBytes, &b) != nil ||
		decodeCanonical(b.Header, 2048, &h) != nil || !h.valid() || len(b.PasswordWrap) != 80 ||
		(h.Recovery == nil && len(b.RecoveryWrap) != 0) || (h.Recovery != nil && len(b.RecoveryWrap) != 80) ||
		len(b.Ciphertext) <= 16 || len(b.Ciphertext) > MaximumScopeBytes+16 || !ed25519.Verify(h.WriterPublic, vaultSignatureMessage(e.Body), e.Signature) {
		return vaultEnvelope{}, vaultBody{}, vaultHeader{}, ErrInvalid
	}
	return e, b, h, nil
}

// InspectPasswordVault checks signed public structure, not credential trust or freshness.
func InspectPasswordVault(raw []byte) (VaultHead, DocumentID, error) {
	_, _, h, err := parseVault(raw)
	if err != nil {
		return VaultHead{}, DocumentID{}, err
	}
	head := h.head()
	head.ID = sha256.Sum256(raw)
	var previous DocumentID
	copy(previous[:], h.Previous)
	return head, previous, nil
}
func PasswordVaultProtection(raw []byte) (VaultProtection, error) {
	_, _, h, err := parseVault(raw)
	if err != nil {
		return VaultProtection{}, err
	}
	return VaultProtection{WriterPublic: h.WriterPublic, SharingPublic: h.SharingPublic, Password: h.Password, RecoveryEpoch: h.RecoveryEpoch, Recovery: h.Recovery}, nil
}

func deriveVaultCredential(ctx context.Context, head VaultHead, d VaultCredential, secret []byte) (hpke.PrivateKey, ed25519.PrivateKey, error) {
	var root []byte
	var err error
	if d.Kind == 1 {
		root, err = passwordRoot(ctx, secret, d.Salt)
	} else {
		if ctx == nil {
			return nil, nil, ErrInvalid
		}
		if err = ctx.Err(); err == nil {
			root, err = parseVaultRecoveryCode(secret)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	defer clear(root)
	info := string(credentialContext(head, d))
	ikm, err := hkdf.Key(sha256.New, root, nil, info+"/hpke", 32)
	if err != nil {
		return nil, nil, err
	}
	defer clear(ikm)
	recipient, err := hpke.DHKEM(ecdh.X25519()).DeriveKeyPair(ikm)
	if err != nil {
		return nil, nil, err
	}
	seed, err := hkdf.Key(sha256.New, root, nil, info+"/sign", 32)
	if err != nil {
		return nil, nil, err
	}
	defer clear(seed)
	return recipient, ed25519.NewKeyFromSeed(seed), nil
}
func makeVaultCredential(ctx context.Context, head VaultHead, kind, epoch uint64, secret, writer []byte) (VaultCredential, error) {
	d := VaultCredential{Kind: kind, Epoch: epoch, Salt: []byte{}}
	if kind == 1 {
		d.Profile = 1
		d.Salt = make([]byte, 16)
		if _, err := rand.Read(d.Salt); err != nil {
			return VaultCredential{}, err
		}
	}
	recipient, signer, err := deriveVaultCredential(ctx, head, d, secret)
	if err != nil {
		return VaultCredential{}, err
	}
	defer clear(signer)
	d.RecipientPublic = recipient.PublicKey().Bytes()
	d.SigningPublic = bytes.Clone(signer.Public().(ed25519.PublicKey))
	d.Delegation = ed25519.Sign(signer, delegationMessage(head, d, writer))
	return d, nil
}
func NewVaultProtection(ctx context.Context, head VaultHead, password, recoveryCode, writerSeed []byte) (VaultProtection, error) {
	if len(writerSeed) != 32 {
		return VaultProtection{}, ErrInvalid
	}
	signer := ed25519.NewKeyFromSeed(writerSeed)
	defer clear(signer)
	p := VaultProtection{WriterPublic: bytes.Clone(signer.Public().(ed25519.PublicKey)), RecoveryEpoch: 1}
	var err error
	p.Password, err = makeVaultCredential(ctx, head, 1, 1, password, p.WriterPublic)
	if err != nil {
		return VaultProtection{}, err
	}
	if len(recoveryCode) > 0 {
		d, err := makeVaultCredential(ctx, head, 2, 1, recoveryCode, p.WriterPublic)
		if err != nil {
			return VaultProtection{}, err
		}
		p.Recovery = &d
	}
	return p, nil
}
func ReplaceVaultPassword(ctx context.Context, head VaultHead, p VaultProtection, password []byte) (VaultProtection, error) {
	d, err := makeVaultCredential(ctx, head, 1, p.Password.Epoch+1, password, p.WriterPublic)
	if err != nil {
		return VaultProtection{}, err
	}
	p.Password = d
	return p, nil
}
func ReplaceVaultRecovery(ctx context.Context, head VaultHead, p VaultProtection, code []byte) (VaultProtection, error) {
	if p.Recovery == nil && len(code) == 0 {
		return p, nil
	}
	if p.RecoveryEpoch == MaximumContractInteger {
		return VaultProtection{}, ErrInvalid
	}
	p.RecoveryEpoch++
	p.Recovery = nil
	if len(code) > 0 {
		d, err := makeVaultCredential(ctx, head, 2, p.RecoveryEpoch, code, p.WriterPublic)
		if err != nil {
			return VaultProtection{}, err
		}
		p.Recovery = &d
	}
	return p, nil
}
func GenerateVaultRecoveryCode() ([]byte, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	defer clear(seed)
	checksum := sha256.Sum256(append([]byte("paperboat.environment.recovery-code/v1"), seed...))
	value := append(bytes.Clone(seed), checksum[:4]...)
	defer clear(value)
	return []byte("pbvr1-" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(value)), nil
}
func parseVaultRecoveryCode(code []byte) ([]byte, error) {
	if len(code) != 64 || !bytes.HasPrefix(code, []byte("pbvr1-")) {
		return nil, ErrVaultUnlock
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(string(code[6:]))
	if err != nil || len(decoded) != 36 {
		clear(decoded)
		return nil, ErrVaultUnlock
	}
	defer clear(decoded)
	if base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded) != string(code[6:]) {
		return nil, ErrVaultUnlock
	}
	checksum := sha256.Sum256(append([]byte("paperboat.environment.recovery-code/v1"), decoded[:32]...))
	if !bytes.Equal(checksum[:4], decoded[32:]) {
		return nil, ErrVaultUnlock
	}
	return bytes.Clone(decoded[:32]), nil
}
func vaultWrapInfo(kind uint64) []byte {
	if kind == 1 {
		return []byte("paperboat.environment.vault-password-key/v1")
	}
	return []byte("paperboat.environment.vault-recovery-key/v1")
}
func wrapVaultKey(d VaultCredential, header, key []byte) ([]byte, error) {
	public, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(d.RecipientPublic)
	if err != nil {
		return nil, ErrInvalid
	}
	enc, sender, err := hpke.NewSender(public, hpke.HKDFSHA256(), hpke.AES256GCM(), vaultWrapInfo(d.Kind))
	if err != nil {
		return nil, ErrInvalid
	}
	sealed, err := sender.Seal(header, key)
	if err != nil {
		return nil, err
	}
	return append(enc, sealed...), nil
}
func vaultAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealProtectedVault uses only unlocked keys and public credential protection.
// It creates a fresh data key; neither a password nor recovery code is retained for writes.
func SealProtectedVault(ctx context.Context, head VaultHead, previous DocumentID, p VaultProtection, payload []byte) ([]byte, VaultHead, error) {
	if ctx == nil {
		return nil, VaultHead{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, VaultHead{}, err
	}
	keys, err := ParseVaultKeys(payload)
	if err != nil {
		return nil, VaultHead{}, err
	}
	defer keys.Clear()
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(signer)
	sharing, err := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
	if err != nil {
		return nil, VaultHead{}, ErrInvalid
	}
	h := vaultHeader{Domain: "paperboat.environment.password-vault", Version: 1, Issuer: head.Issuer, AccountID: head.AccountID, Generation: head.Generation, Previous: previous[:], WriterPublic: p.WriterPublic, Password: p.Password, RecoveryEpoch: p.RecoveryEpoch, Recovery: p.Recovery, Nonce: make([]byte, 12), SharingPublic: sharing.PublicKey().Bytes()}
	if !h.valid() || head.ID != (DocumentID{}) || !bytes.Equal(signer.Public().(ed25519.PublicKey), p.WriterPublic) {
		return nil, VaultHead{}, ErrInvalid
	}
	if _, err := rand.Read(h.Nonce); err != nil {
		return nil, VaultHead{}, err
	}
	header, err := encode(h)
	if err != nil {
		return nil, VaultHead{}, err
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return nil, VaultHead{}, err
	}
	aead, err := vaultAEAD(key)
	if err != nil {
		return nil, VaultHead{}, err
	}
	b := vaultBody{Header: header, RecoveryWrap: []byte{}, Ciphertext: aead.Seal(nil, h.Nonce, payload, header)}
	b.PasswordWrap, err = wrapVaultKey(p.Password, header, key)
	if err != nil {
		return nil, VaultHead{}, err
	}
	if p.Recovery != nil {
		b.RecoveryWrap, err = wrapVaultKey(*p.Recovery, header, key)
		if err != nil {
			return nil, VaultHead{}, err
		}
	}
	body, err := encode(b)
	if err != nil {
		return nil, VaultHead{}, err
	}
	raw, err := encode(vaultEnvelope{Body: body, Signature: ed25519.Sign(signer, vaultSignatureMessage(body))})
	if err != nil || len(raw) > MaximumVaultBytes {
		return nil, VaultHead{}, ErrInvalid
	}
	head.ID = sha256.Sum256(raw)
	return raw, head, nil
}
func SealPasswordVault(ctx context.Context, head VaultHead, previous DocumentID, password, payload []byte) ([]byte, VaultHead, error) {
	keys, err := ParseVaultKeys(payload)
	if err != nil {
		return nil, VaultHead{}, err
	}
	defer keys.Clear()
	p, err := NewVaultProtection(ctx, head, password, nil, keys.WriterSeed)
	if err != nil {
		return nil, VaultHead{}, err
	}
	return SealProtectedVault(ctx, head, previous, p, payload)
}
func OpenPasswordVault(ctx context.Context, expected VaultHead, password, raw []byte) ([]byte, error) {
	return openCredentialVault(ctx, expected, password, raw, 1)
}
func OpenRecoveryVault(ctx context.Context, expected VaultHead, code, raw []byte) ([]byte, error) {
	return openCredentialVault(ctx, expected, code, raw, 2)
}
func openCredentialVault(ctx context.Context, expected VaultHead, secret, raw []byte, kind uint64) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaximumVaultBytes || expected.ID == (DocumentID{}) || DocumentID(sha256.Sum256(raw)) != expected.ID {
		return nil, ErrVaultUnlock
	}
	e, b, h, err := parseVault(raw)
	if err != nil || h.Issuer != expected.Issuer || h.AccountID != expected.AccountID || h.Generation != expected.Generation {
		return nil, ErrVaultUnlock
	}
	d := h.Password
	wrapped := b.PasswordWrap
	if kind == 2 {
		if h.Recovery == nil {
			return nil, ErrVaultUnlock
		}
		d = *h.Recovery
		wrapped = b.RecoveryWrap
	}
	recipient, signer, err := deriveVaultCredential(ctx, expected, d, secret)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, ErrVaultUnlock
	}
	defer clear(signer)
	if !bytes.Equal(recipient.PublicKey().Bytes(), d.RecipientPublic) || !bytes.Equal(signer.Public().(ed25519.PublicKey), d.SigningPublic) ||
		!ed25519.Verify(signer.Public().(ed25519.PublicKey), delegationMessage(expected, d, h.WriterPublic), d.Delegation) ||
		!ed25519.Verify(h.WriterPublic, vaultSignatureMessage(e.Body), e.Signature) {
		return nil, ErrVaultUnlock
	}
	receiver, err := hpke.NewRecipient(wrapped[:32], recipient, hpke.HKDFSHA256(), hpke.AES256GCM(), vaultWrapInfo(kind))
	if err != nil {
		return nil, ErrVaultUnlock
	}
	key, err := receiver.Open(b.Header, wrapped[32:])
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, ErrVaultUnlock
	}
	defer clear(key)
	aead, err := vaultAEAD(key)
	if err != nil {
		return nil, ErrVaultUnlock
	}
	payload, err := aead.Open(nil, h.Nonce, b.Ciphertext, b.Header)
	if err != nil {
		return nil, ErrVaultUnlock
	}
	keys, err := ParseVaultKeys(payload)
	if err != nil {
		clear(payload)
		return nil, ErrVaultUnlock
	}
	defer keys.Clear()
	writer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(writer)
	sharing, sharingErr := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
	if sharingErr != nil || !bytes.Equal(sharing.PublicKey().Bytes(), h.SharingPublic) || !bytes.Equal(writer.Public().(ed25519.PublicKey), h.WriterPublic) {
		clear(payload)
		return nil, ErrVaultUnlock
	}
	if err := ctx.Err(); err != nil {
		clear(payload)
		return nil, err
	}
	return payload, nil
}
