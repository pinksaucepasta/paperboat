package environmente2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"sort"
)

func BuildManifest(input BuildManifestInput) ([]byte, error) {
	random := input.Random
	if random == nil {
		random = rand.Reader
	}
	c := input.Claims
	if len(input.ScopeKey) != 32 || len(input.SignerPrivate) != ed25519.PrivateKeySize {
		return nil, ErrInvalid
	}
	signerID, err := KeyIDEd25519(input.SignerPrivate.Public().(ed25519.PublicKey))
	if err != nil || signerID != input.SignerKeyID {
		return nil, ErrInvalid
	}
	names, plaintext, err := encodeScope(input.Values)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	c.Names = names
	c.Salt = make([]byte, 32)
	c.Nonce = make([]byte, 12)
	if _, err = io.ReadFull(random, c.Salt); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(random, c.Nonce); err != nil {
		return nil, err
	}
	padded, err := padPlaintext(random, plaintext)
	if err != nil {
		return nil, err
	}
	defer clear(padded)
	key, err := payloadKey(c, input.ScopeKey)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	aad, err := payloadAAD(c)
	if err != nil {
		return nil, err
	}
	c.Ciphertext = aead.Seal(nil, c.Nonce, padded, aad)
	digest := sha256.Sum256(c.Ciphertext)
	c.CiphertextSHA256 = DocumentID(digest)
	recipients := append([]Recipient(nil), input.Recipients...)
	sort.Slice(recipients, func(i, j int) bool { return recipientLess(recipients[i], recipients[j]) })
	c.Wraps = make([]RecipientWrap, len(recipients))
	for i, r := range recipients {
		wrap, e := WrapScopeKey(c, r, input.ScopeKey)
		if e != nil {
			return nil, e
		}
		c.Wraps[i] = wrap
	}
	if err = validateManifestClaims(c, nil); err != nil {
		return nil, err
	}
	body, err := encode(manifestArray(c))
	if err != nil {
		return nil, err
	}
	return signDocument(contentManifest, input.SignerKeyID, body, input.SignerPrivate)
}

func ParseManifest(raw []byte, authority Authority) (Manifest, error) {
	doc, err := parseDocument(raw, MaximumManifestBytes, contentManifest)
	if err != nil {
		return Manifest{}, err
	}
	var signer ed25519.PublicKey
	for _, binding := range authority.Bindings {
		if (binding.SubjectKind == SubjectManagerCLI || binding.SubjectKind == SubjectManagerBrowser) && binding.SigningKeyID == doc.KeyID {
			signer = binding.SigningPublic
			break
		}
	}
	if signer == nil || verifyDocument(doc, signer) != nil {
		return Manifest{}, ErrInvalid
	}
	claims, err := parseManifestBody(doc.Body)
	if err != nil {
		return Manifest{}, err
	}
	if claims.AccountID != authority.AccountID || claims.AuthorityGeneration != authority.Generation || claims.AuthorityID != authority.ID || validateManifestClaims(claims, &authority) != nil {
		return Manifest{}, ErrInvalid
	}
	return Manifest{ManifestClaims: claims, SignerKeyID: doc.KeyID, ID: DocumentID(documentDigest(raw)), Raw: cloneBytes(raw)}, nil
}

func DecryptManifest(manifest Manifest, recipient RecipientPrivate) (DecryptedScope, error) {
	var selected *RecipientWrap
	for i := range manifest.Wraps {
		w := &manifest.Wraps[i]
		if w.Kind == recipient.Kind && w.SubjectID == recipient.SubjectID && w.KeyGeneration == recipient.KeyGeneration && w.KeyID == recipient.KeyID {
			if selected != nil {
				return DecryptedScope{}, ErrInvalid
			}
			selected = w
		}
	}
	if selected == nil {
		return DecryptedScope{}, ErrInvalid
	}
	scopeKey, err := OpenScopeKey(manifest.ManifestClaims, *selected, recipient.PrivateKey)
	if err != nil {
		return DecryptedScope{}, ErrInvalid
	}
	key, err := payloadKey(manifest.ManifestClaims, scopeKey)
	if err != nil {
		clear(scopeKey)
		return DecryptedScope{}, ErrInvalid
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		clear(scopeKey)
		return DecryptedScope{}, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		clear(scopeKey)
		return DecryptedScope{}, ErrInvalid
	}
	aad, err := payloadAAD(manifest.ManifestClaims)
	if err != nil {
		clear(scopeKey)
		return DecryptedScope{}, ErrInvalid
	}
	padded, err := aead.Open(nil, manifest.Nonce, manifest.Ciphertext, aad)
	if err != nil {
		clear(scopeKey)
		return DecryptedScope{}, ErrInvalid
	}
	plain, err := unpadPlaintext(padded)
	clear(padded)
	if err != nil {
		clear(scopeKey)
		return DecryptedScope{}, ErrInvalid
	}
	values, names, err := decodeScope(plain)
	clear(plain)
	if err != nil || !equalStrings(names, manifest.Names) {
		clear(scopeKey)
		clearValues(values)
		return DecryptedScope{}, ErrInvalid
	}
	return DecryptedScope{Values: values, ScopeKey: scopeKey}, nil
}

func WrapScopeKey(c ManifestClaims, recipient Recipient, scopeKey []byte) (RecipientWrap, error) {
	if len(scopeKey) != 32 || validateRecipient(recipient) != nil {
		return RecipientWrap{}, ErrInvalid
	}
	public, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(recipient.PublicKey)
	if err != nil {
		return RecipientWrap{}, ErrInvalid
	}
	info, err := hpkeInfo(c)
	if err != nil {
		return RecipientWrap{}, err
	}
	aad, err := wrapAAD(c, recipient.Kind, recipient.SubjectID, recipient.KeyGeneration, recipient.KeyID)
	if err != nil {
		return RecipientWrap{}, err
	}
	enc, sender, err := hpke.NewSender(public, hpke.HKDFSHA256(), hpke.AES256GCM(), info)
	if err != nil {
		return RecipientWrap{}, ErrInvalid
	}
	ciphertext, err := sender.Seal(aad, scopeKey)
	if err != nil {
		return RecipientWrap{}, ErrInvalid
	}
	if len(enc) != 32 || len(ciphertext) != 48 {
		return RecipientWrap{}, ErrInvalid
	}
	return RecipientWrap{Kind: recipient.Kind, SubjectID: recipient.SubjectID, KeyGeneration: recipient.KeyGeneration, KeyID: recipient.KeyID, EncapsulatedKey: enc, Ciphertext: ciphertext}, nil
}

func OpenScopeKey(c ManifestClaims, wrap RecipientWrap, privateBytes []byte) ([]byte, error) {
	if len(privateBytes) != 32 || len(wrap.EncapsulatedKey) != 32 || len(wrap.Ciphertext) != 48 {
		return nil, ErrInvalid
	}
	private, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(privateBytes)
	if err != nil {
		return nil, ErrInvalid
	}
	public := private.PublicKey().Bytes()
	kid, err := KeyIDX25519(public)
	if err != nil || kid != wrap.KeyID {
		return nil, ErrInvalid
	}
	info, err := hpkeInfo(c)
	if err != nil {
		return nil, err
	}
	aad, err := wrapAAD(c, wrap.Kind, wrap.SubjectID, wrap.KeyGeneration, wrap.KeyID)
	if err != nil {
		return nil, err
	}
	receiver, err := hpke.NewRecipient(wrap.EncapsulatedKey, private, hpke.HKDFSHA256(), hpke.AES256GCM(), info)
	if err != nil {
		return nil, ErrInvalid
	}
	plain, err := receiver.Open(aad, wrap.Ciphertext)
	if err != nil || len(plain) != 32 {
		clear(plain)
		return nil, ErrInvalid
	}
	return plain, nil
}

func payloadKey(c ManifestClaims, scopeKey []byte) ([]byte, error) {
	info, err := encode([]any{"paperboat.environment.payload-key", uint64(1), c.AccountID, c.AuthorityID[:], uint64(c.Scope), machineValue(c), uint64(c.State), c.PreviousVersion, c.Version, c.KeyEpoch, c.OperationID[:]})
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, scopeKey, c.Salt, string(info), 32)
}
func payloadAAD(c ManifestClaims) ([]byte, error) {
	return encode([]any{"paperboat.environment.payload-aad", uint64(1), uint64(1), c.AccountID, c.AuthorityGeneration, c.AuthorityID[:], uint64(c.Scope), machineValue(c), uint64(c.State), c.PreviousVersion, c.Version, c.KeyEpoch, c.OperationID[:], c.Salt, c.Nonce, uint64(c.Mutation), stringsAny(c.ChangedNames), stringsAny(c.Names)})
}
func hpkeInfo(c ManifestClaims) ([]byte, error) {
	return encode([]any{"paperboat.environment.hpke-info", uint64(1), uint64(1), c.AccountID, c.AuthorityID[:], uint64(c.Scope), machineValue(c), uint64(c.State), c.KeyEpoch, c.Version, c.OperationID[:]})
}
func wrapAAD(c ManifestClaims, kind RecipientKind, subject string, generation uint64, kid string) ([]byte, error) {
	return encode([]any{"paperboat.environment.wrap-aad", uint64(1), uint64(1), c.AccountID, c.AuthorityGeneration, c.AuthorityID[:], uint64(c.Scope), machineValue(c), uint64(c.State), c.PreviousVersion, c.Version, c.KeyEpoch, c.OperationID[:], c.Salt, c.CiphertextSHA256[:], uint64(kind), subject, generation, kid})
}

func manifestArray(c ManifestClaims) []any {
	wraps := make([]any, len(c.Wraps))
	for i, w := range c.Wraps {
		wraps[i] = []any{uint64(w.Kind), w.SubjectID, w.KeyGeneration, w.KeyID, w.EncapsulatedKey, w.Ciphertext}
	}
	return []any{"paperboat.environment.scope-manifest", uint64(1), uint64(1), c.AccountID, c.AuthorityGeneration, c.AuthorityID[:], uint64(c.Scope), machineValue(c), uint64(c.State), c.PreviousVersion, c.Version, c.KeyEpoch, c.OperationID[:], c.Salt, c.Nonce, uint64(c.Mutation), stringsAny(c.ChangedNames), stringsAny(c.Names), c.CiphertextSHA256[:], c.Ciphertext, wraps}
}

func parseManifestBody(raw []byte) (ManifestClaims, error) {
	var v any
	if decodeCanonical(raw, MaximumManifestBytes, &v) != nil {
		return ManifestClaims{}, ErrInvalid
	}
	a, e := array(v, 21)
	if e != nil || requireDomain(a, "paperboat.environment.scope-manifest", 1) != nil {
		return ManifestClaims{}, ErrInvalid
	}
	profile, e0 := uintValue(a[2], false)
	account, e1 := text(a[3])
	agen, e2 := uintValue(a[4], false)
	aid, e3 := bytesValue(a[5], 32)
	scope, e4 := uintValue(a[6], true)
	machine, e5 := nullableText(a[7])
	state, e6 := uintValue(a[8], true)
	prev, e7 := uintValue(a[9], true)
	version, e8 := uintValue(a[10], false)
	epoch, e9 := uintValue(a[11], false)
	op, e10 := bytesValue(a[12], 16)
	salt, e11 := bytesValue(a[13], 32)
	nonce, e12 := bytesValue(a[14], 12)
	mutation, e13 := uintValue(a[15], true)
	changed, e14 := parseStringArray(a[16])
	names, e15 := parseStringArray(a[17])
	hash, e16 := bytesValue(a[18], 32)
	ciphertext, e17 := bytesValue(a[19], -1)
	wrapValues, e18 := arrayAny(a[20])
	if anyError(e0, e1, e2, e3, e4, e5, e6, e7, e8, e9, e10, e11, e12, e13, e14, e15, e16, e17, e18) || profile != 1 {
		return ManifestClaims{}, ErrInvalid
	}
	c := ManifestClaims{AccountID: account, AuthorityGeneration: agen, Scope: ScopeKind(scope), State: ScopeState(state), PreviousVersion: prev, Version: version, KeyEpoch: epoch, Salt: salt, Nonce: nonce, Mutation: MutationKind(mutation), ChangedNames: changed, Names: names, Ciphertext: ciphertext}
	copy(c.AuthorityID[:], aid)
	copy(c.OperationID[:], op)
	copy(c.CiphertextSHA256[:], hash)
	if machine != nil {
		c.MachineID = *machine
	}
	c.Wraps = make([]RecipientWrap, len(wrapValues))
	for i, x := range wrapValues {
		p, e := array(x, 6)
		if e != nil {
			return ManifestClaims{}, ErrInvalid
		}
		kind, e1 := uintValue(p[0], false)
		subject, e2 := text(p[1])
		gen, e3 := uintValue(p[2], false)
		kid, e4 := text(p[3])
		enc, e5 := bytesValue(p[4], 32)
		ct, e6 := bytesValue(p[5], 48)
		if anyError(e1, e2, e3, e4, e5, e6) {
			return ManifestClaims{}, ErrInvalid
		}
		c.Wraps[i] = RecipientWrap{Kind: RecipientKind(kind), SubjectID: subject, KeyGeneration: gen, KeyID: kid, EncapsulatedKey: enc, Ciphertext: ct}
	}
	return c, nil
}

func validateManifestClaims(c ManifestClaims, authority *Authority) error {
	if !validIdentifier(c.AccountID) || c.AuthorityGeneration == 0 || c.Version == 0 || c.Version > MaximumContractInteger || c.PreviousVersion > MaximumContractInteger || c.KeyEpoch == 0 || c.KeyEpoch > MaximumContractInteger || allZero(c.OperationID[:]) || len(c.Salt) != 32 || len(c.Nonce) != 12 || c.Scope > ScopeMachine || c.State > ScopeRetired || c.Mutation > MutationReset {
		return ErrInvalid
	}
	if c.Scope == ScopeGlobal {
		if c.MachineID != "" || c.State != ScopeActive {
			return ErrInvalid
		}
	} else if !validIdentifier(c.MachineID) {
		return ErrInvalid
	}
	if validateNames(c.Names) != nil || validateNames(c.ChangedNames) != nil {
		return ErrInvalid
	}
	digest := sha256.Sum256(c.Ciphertext)
	if DocumentID(digest) != c.CiphertextSHA256 || !validCiphertextLength(len(c.Ciphertext)) {
		return ErrInvalid
	}
	if len(c.Wraps) == 0 || len(c.Wraps) > MaximumManagers+MaximumHosts+1 {
		return ErrInvalid
	}
	for i, w := range c.Wraps {
		if validateWrap(w) != nil {
			return ErrInvalid
		}
		if i > 0 && !wrapLess(c.Wraps[i-1], w) {
			return ErrInvalid
		}
	}
	if authority != nil {
		if c.Scope == ScopeMachine {
			hasTargetHost := false
			for _, binding := range authority.Bindings {
				if binding.SubjectKind == SubjectHost && binding.SubjectID == c.MachineID {
					hasTargetHost = true
					break
				}
			}
			if hasTargetHost != (c.State == ScopeActive) {
				return ErrInvalid
			}
		}
		expected, err := authority.ExpectedRecipients(c.Scope, c.State, c.MachineID)
		if err != nil || !equalWrapRecipients(c.Wraps, expected) {
			return ErrInvalid
		}
	}
	return nil
}
