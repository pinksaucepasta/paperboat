package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

const (
	testVaultIssuer   = "https://control.example"
	testVaultAccount  = "account_1"
	testVaultPassword = "test master password"
)

type testVault struct {
	Head         VaultHead
	Keys         VaultKeys
	Payload      []byte
	Password     []byte
	RecoveryCode []byte
	Protection   VaultProtection
	Envelope     []byte
}

func newTestVault(t *testing.T, withRecovery bool) testVault {
	t.Helper()
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys.Teams = []VaultTeamKey{{
		TeamID:               "team_1",
		Epoch:                1,
		MembershipGeneration: 4,
		Key:                  bytes.Repeat([]byte{0x31}, 32),
	}}
	payload, err := keys.MarshalBinary()
	if err != nil {
		keys.Clear()
		t.Fatal(err)
	}
	password := []byte(testVaultPassword)
	var recoveryCode []byte
	if withRecovery {
		recoveryCode, err = GenerateVaultRecoveryCode()
		if err != nil {
			keys.Clear()
			clear(payload)
			t.Fatal(err)
		}
	}
	head := VaultHead{Issuer: testVaultIssuer, AccountID: testVaultAccount, Generation: 1}
	protection, err := NewVaultProtection(context.Background(), head, password, recoveryCode, keys.WriterSeed)
	if err != nil {
		keys.Clear()
		clear(payload)
		clear(recoveryCode)
		t.Fatal(err)
	}
	envelope, sealedHead, err := SealProtectedVault(context.Background(), head, DocumentID{}, protection, payload)
	if err != nil {
		keys.Clear()
		clear(payload)
		clear(recoveryCode)
		t.Fatal(err)
	}
	keys.Clear()
	return testVault{
		Head:         sealedHead,
		Payload:      payload,
		Password:     password,
		RecoveryCode: recoveryCode,
		Protection:   protection,
		Envelope:     envelope,
	}
}

func clearTestVault(v testVault) {
	clear(v.Payload)
	clear(v.Password)
	clear(v.RecoveryCode)
	clear(v.Envelope)
	clear(v.Protection.WriterPublic)
	clear(v.Protection.Password.Salt)
	clear(v.Protection.Password.RecipientPublic)
	clear(v.Protection.Password.SigningPublic)
	clear(v.Protection.Password.Delegation)
	if v.Protection.Recovery != nil {
		clear(v.Protection.Recovery.Salt)
		clear(v.Protection.Recovery.RecipientPublic)
		clear(v.Protection.Recovery.SigningPublic)
		clear(v.Protection.Recovery.Delegation)
	}
	v.Keys.Clear()
}

func nextVaultHead(head VaultHead) VaultHead {
	return VaultHead{Issuer: head.Issuer, AccountID: head.AccountID, Generation: head.Generation + 1}
}

func sealTestSuccessor(t *testing.T, current VaultHead, protection VaultProtection, payload []byte) ([]byte, VaultHead) {
	t.Helper()
	raw, head, err := SealProtectedVault(context.Background(), nextVaultHead(current), current.ID, protection, payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw, head
}

func requireUnlockError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrVaultUnlock) {
		t.Fatalf("want ErrVaultUnlock, got %v", err)
	}
}

func decodeTestEnvelope(t *testing.T, raw []byte) (vaultEnvelope, vaultBody, vaultHeader) {
	t.Helper()
	var envelope vaultEnvelope
	if err := decodeCanonical(raw, MaximumVaultBytes, &envelope); err != nil {
		t.Fatal(err)
	}
	var body vaultBody
	if err := decodeCanonical(envelope.Body, MaximumVaultBytes, &body); err != nil {
		t.Fatal(err)
	}
	var header vaultHeader
	if err := decodeCanonical(body.Header, 2048, &header); err != nil {
		t.Fatal(err)
	}
	return envelope, body, header
}

func encodeTestEnvelope(t *testing.T, envelope vaultEnvelope, body vaultBody, header vaultHeader) []byte {
	t.Helper()
	var err error
	body.Header, err = encode(header)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Body, err = encode(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encode(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func expectedHead(raw []byte, head VaultHead) VaultHead {
	head.ID = sha256.Sum256(raw)
	return head
}

func unwrapTestDataKey(t *testing.T, head VaultHead, password, raw []byte) []byte {
	t.Helper()
	envelope, body, header := decodeTestEnvelope(t, raw)
	recipient, signer, err := deriveVaultCredential(context.Background(), head, header.Password, password)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(signer)
	recipientBytes, err := recipient.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(recipientBytes)
	wrapped := body.PasswordWrap
	receiver, err := hpke.NewRecipient(wrapped[:32], recipient, hpke.HKDFSHA256(), hpke.AES256GCM(), vaultWrapInfo(1))
	if err != nil {
		t.Fatal(err)
	}
	key, err := receiver.Open(body.Header, wrapped[32:])
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || len(key) != 32 {
		t.Fatal("invalid test envelope key")
	}
	return key
}

func TestPasswordVaultSharedVector(t *testing.T) {
	data, err := os.ReadFile("../../testdata/contracts/environment-e2ee-v1/password-vault.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Issuer       string `json:"issuer"`
		AccountID    string `json:"account_id"`
		Generation   uint64 `json:"generation"`
		DocumentID   string `json:"document_id"`
		Envelope     string `json:"envelope"`
		Password     string `json:"password"`
		Payload      string `json:"payload"`
		RecoveryCode string `json:"recovery_code"`
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	if vector.Issuer == "" || vector.AccountID == "" || vector.Generation == 0 || vector.Password == "" || vector.RecoveryCode == "" {
		t.Fatal("shared vector omitted required public credentials")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(vector.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseDocumentID(vector.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(vector.Payload)
	if err != nil {
		t.Fatal(err)
	}
	head := VaultHead{Issuer: vector.Issuer, AccountID: vector.AccountID, Generation: vector.Generation, ID: id}
	parsed, previous, err := InspectPasswordVault(raw)
	if err != nil || parsed != head || previous != (DocumentID{}) {
		t.Fatalf("shared header mismatch: parsed=%+v previous=%s err=%v", parsed, previous, err)
	}
	keys, err := ParseVaultKeys(payload)
	if err != nil {
		t.Fatal("shared payload is not a VaultKeys document")
	}
	if len(keys.WriterSeed) != 32 || len(keys.PersonalKey) != 32 || len(keys.SharingPrivate) != 32 {
		keys.Clear()
		t.Fatal("shared payload omitted native vault key material")
	}
	keys.Clear()
	opened, err := OpenPasswordVault(context.Background(), head, []byte(vector.Password), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(opened)
	if !bytes.Equal(opened, payload) {
		t.Fatal("shared password plaintext mismatch")
	}
	openedRecovery, err := OpenRecoveryVault(context.Background(), head, []byte(vector.RecoveryCode), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(openedRecovery)
	if !bytes.Equal(openedRecovery, payload) {
		t.Fatal("shared recovery plaintext mismatch")
	}
}

func TestPasswordVaultLatestCredentialsAfterPublicUpdates(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	originalPayload := bytes.Clone(v.Payload)
	originalPassword := bytes.Clone(v.Password)
	originalCode := bytes.Clone(v.RecoveryCode)
	defer clear(originalPayload)
	defer clear(originalPassword)
	defer clear(originalCode)

	newPassword := []byte("replacement master password")
	defer clear(newPassword)
	p1, err := ReplaceVaultPassword(context.Background(), v.Head, v.Protection, newPassword)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Recovery == nil || p1.Recovery.Epoch != v.Protection.Recovery.Epoch {
		t.Fatal("password replacement discarded recovery protection")
	}
	raw1, head1 := sealTestSuccessor(t, v.Head, p1, v.Payload)
	defer clear(raw1)
	if opened, err := OpenPasswordVault(context.Background(), head1, newPassword, raw1); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if opened, err := OpenRecoveryVault(context.Background(), head1, v.RecoveryCode, raw1); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if _, err := OpenPasswordVault(context.Background(), head1, v.Password, raw1); !errors.Is(err, ErrVaultUnlock) {
		t.Fatal("replaced password still unlocked latest vault")
	}

	newCode, err := GenerateVaultRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(newCode)
	p2, err := ReplaceVaultRecovery(context.Background(), head1, p1, newCode)
	if err != nil {
		t.Fatal(err)
	}
	raw2, head2 := sealTestSuccessor(t, head1, p2, v.Payload)
	defer clear(raw2)
	if opened, err := OpenPasswordVault(context.Background(), head2, newPassword, raw2); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if opened, err := OpenRecoveryVault(context.Background(), head2, newCode, raw2); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if _, err := OpenRecoveryVault(context.Background(), head2, v.RecoveryCode, raw2); !errors.Is(err, ErrVaultUnlock) {
		t.Fatal("replaced recovery code still unlocked latest vault")
	}
	if opened, err := OpenRecoveryVault(context.Background(), v.Head, originalCode, v.Envelope); err != nil || !bytes.Equal(opened, originalPayload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if !bytes.Equal(v.Password, originalPassword) {
		t.Fatal("password replacement mutated the caller password")
	}
}

func TestPasswordVaultRecoveryReplacementDisableAndOldSnapshot(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	oldRaw := bytes.Clone(v.Envelope)
	oldHead := v.Head
	oldCode := bytes.Clone(v.RecoveryCode)
	defer clear(oldRaw)
	defer clear(oldCode)
	newCode, err := GenerateVaultRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(newCode)
	p1, err := ReplaceVaultRecovery(context.Background(), oldHead, v.Protection, newCode)
	if err != nil {
		t.Fatal(err)
	}
	replacedRaw, replacedHead := sealTestSuccessor(t, oldHead, p1, v.Payload)
	defer clear(replacedRaw)
	if _, err := OpenRecoveryVault(context.Background(), replacedHead, oldCode, replacedRaw); !errors.Is(err, ErrVaultUnlock) {
		t.Fatal("obsolete recovery code unlocked replaced snapshot")
	}
	if opened, err := OpenRecoveryVault(context.Background(), replacedHead, newCode, replacedRaw); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if opened, err := OpenRecoveryVault(context.Background(), oldHead, oldCode, oldRaw); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}

	disabled, err := ReplaceVaultRecovery(context.Background(), replacedHead, p1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Recovery != nil || disabled.RecoveryEpoch != p1.RecoveryEpoch+1 {
		t.Fatal("recovery disable did not advance epoch and clear descriptor")
	}
	disabledRaw, disabledHead := sealTestSuccessor(t, replacedHead, disabled, v.Payload)
	defer clear(disabledRaw)
	if opened, err := OpenPasswordVault(context.Background(), disabledHead, v.Password, disabledRaw); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
	if _, err := OpenRecoveryVault(context.Background(), disabledHead, newCode, disabledRaw); !errors.Is(err, ErrVaultUnlock) {
		t.Fatal("disabled recovery code unlocked latest snapshot")
	}
	if opened, err := OpenRecoveryVault(context.Background(), replacedHead, newCode, replacedRaw); err != nil || !bytes.Equal(opened, v.Payload) {
		requireUnlockError(t, err)
	} else {
		clear(opened)
	}
}

func TestPasswordVaultMalformedAndWrongRecoveryCodes(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	checksumCode := bytes.Clone(v.RecoveryCode[:len(v.RecoveryCode)-1])
	last := v.RecoveryCode[len(v.RecoveryCode)-1]
	if last == 'A' {
		checksumCode = append(checksumCode, 'B')
	} else {
		checksumCode = append(checksumCode, 'A')
	}
	lowercaseCode := bytes.ToLower(bytes.Clone(v.RecoveryCode))
	if bytes.Equal(lowercaseCode, v.RecoveryCode) {
		lowercaseCode[6] = 'a'
	}
	cases := map[string][]byte{
		"empty":     nil,
		"wrong":     []byte("pbvr1-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
		"lowercase": lowercaseCode,
		"checksum":  checksumCode,
		"prefix":    append([]byte("pbvr0-"), v.RecoveryCode[6:]...),
		"short":     v.RecoveryCode[:len(v.RecoveryCode)-1],
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenRecoveryVault(context.Background(), v.Head, code, v.Envelope); !errors.Is(err, ErrVaultUnlock) {
				t.Fatalf("malformed/wrong code returned %v", err)
			}
		})
	}
	keys, err := ParseVaultKeys(v.Payload)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	if _, err := NewVaultProtection(context.Background(), v.Head, v.Password, []byte("not-a-code"), keys.WriterSeed); !errors.Is(err, ErrVaultUnlock) {
		t.Fatalf("malformed code accepted during protection creation: %v", err)
	}
}

func TestPasswordVaultRejectsHeaderBodyWrapAndSignatureTamperWithRecomputedHead(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	cases := []struct {
		name string
		edit func(vaultEnvelope, vaultBody, vaultHeader) (vaultEnvelope, vaultBody, vaultHeader)
	}{
		{name: "header", edit: func(e vaultEnvelope, b vaultBody, h vaultHeader) (vaultEnvelope, vaultBody, vaultHeader) {
			h.Nonce[0] ^= 1
			return e, b, h
		}},
		{name: "body", edit: func(e vaultEnvelope, b vaultBody, h vaultHeader) (vaultEnvelope, vaultBody, vaultHeader) {
			b.Ciphertext[0] ^= 1
			return e, b, h
		}},
		{name: "password-wrap", edit: func(e vaultEnvelope, b vaultBody, h vaultHeader) (vaultEnvelope, vaultBody, vaultHeader) {
			b.PasswordWrap[0] ^= 1
			return e, b, h
		}},
		{name: "signature", edit: func(e vaultEnvelope, b vaultBody, h vaultHeader) (vaultEnvelope, vaultBody, vaultHeader) {
			e.Signature[0] ^= 1
			return e, b, h
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, b, h := decodeTestEnvelope(t, v.Envelope)
			e, b, h = tc.edit(e, b, h)
			tampered := encodeTestEnvelope(t, e, b, h)
			if _, err := OpenPasswordVault(context.Background(), expectedHead(tampered, v.Head), v.Password, tampered); !errors.Is(err, ErrVaultUnlock) {
				t.Fatalf("tampered %s accepted: %v", tc.name, err)
			}
			clear(tampered)
		})
	}
}

func TestPasswordVaultPublicChosenWriterForgeryRejectedByDelegation(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	e, b, h := decodeTestEnvelope(t, v.Envelope)
	_, attackerPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(attackerPrivate)
	h.WriterPublic = bytes.Clone(attackerPrivate.Public().(ed25519.PublicKey))
	header, err := encode(h)
	if err != nil {
		t.Fatal(err)
	}
	b.Header = header
	body, err := encode(b)
	if err != nil {
		t.Fatal(err)
	}
	e.Body = body
	e.Signature = ed25519.Sign(attackerPrivate, vaultSignatureMessage(body))
	forged, err := encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := parseVault(forged); err == nil {
		t.Fatal("public-only chosen writer passed credential delegation validation")
	}
	if _, err := OpenPasswordVault(context.Background(), expectedHead(forged, v.Head), v.Password, forged); !errors.Is(err, ErrVaultUnlock) {
		t.Fatalf("chosen-writer forgery returned %v", err)
	}
	clear(forged)
}

func TestPasswordVaultFreshDataKeyAndCallerBuffers(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	payloadBefore := bytes.Clone(v.Payload)
	passwordBefore := bytes.Clone(v.Password)
	recoveryBefore := bytes.Clone(v.RecoveryCode)
	defer clear(payloadBefore)
	defer clear(passwordBefore)
	defer clear(recoveryBefore)
	firstRaw := bytes.Clone(v.Envelope)
	secondRaw, secondHead := sealTestSuccessor(t, v.Head, v.Protection, v.Payload)
	defer clear(secondRaw)
	firstKey := unwrapTestDataKey(t, v.Head, v.Password, firstRaw)
	secondKey := unwrapTestDataKey(t, secondHead, v.Password, secondRaw)
	defer clear(firstKey)
	defer clear(secondKey)
	if bytes.Equal(firstKey, secondKey) {
		t.Fatal("successive vault writes reused the data-encryption key")
	}
	if bytes.Equal(firstRaw, secondRaw) {
		t.Fatal("successive vault writes produced identical envelopes")
	}
	if !bytes.Equal(v.Payload, payloadBefore) || !bytes.Equal(v.Password, passwordBefore) || !bytes.Equal(v.RecoveryCode, recoveryBefore) {
		t.Fatal("vault write mutated caller buffers")
	}
	clear(firstRaw)
}

func TestPasswordVaultWriterMismatchAndTeamKeyUpdate(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	parsed, err := ParseVaultKeys(v.Payload)
	if err != nil {
		t.Fatal(err)
	}
	wrongWriter := bytes.Repeat([]byte{0xa7}, 32)
	if bytes.Equal(wrongWriter, parsed.WriterSeed) {
		wrongWriter[0] ^= 1
	}
	parsed.WriterSeed = wrongWriter
	wrongPayload, err := parsed.MarshalBinary()
	if err != nil {
		parsed.Clear()
		t.Fatal(err)
	}
	if _, _, err := SealProtectedVault(context.Background(), v.Head, DocumentID{}, v.Protection, wrongPayload); !errors.Is(err, ErrInvalid) {
		t.Fatalf("writer-mismatched payload returned %v", err)
	}
	parsed.Clear()
	clear(wrongPayload)
	clear(wrongWriter)

	updated, err := ParseVaultKeys(v.Payload)
	if err != nil {
		t.Fatal(err)
	}
	updated.Teams[0].Epoch++
	updated.Teams[0].MembershipGeneration++
	updated.Teams[0].Key = bytes.Repeat([]byte{0x62}, 32)
	updatedPayload, err := updated.MarshalBinary()
	if err != nil {
		updated.Clear()
		t.Fatal(err)
	}
	updated.Clear()
	updatedRaw, updatedHead := sealTestSuccessor(t, v.Head, v.Protection, updatedPayload)
	defer clear(updatedRaw)
	opened, err := OpenPasswordVault(context.Background(), updatedHead, v.Password, updatedRaw)
	if err != nil {
		clear(updatedPayload)
		t.Fatal(err)
	}
	defer clear(opened)
	openedKeys, err := ParseVaultKeys(opened)
	if err != nil {
		clear(updatedPayload)
		t.Fatal(err)
	}
	defer openedKeys.Clear()
	if openedKeys.Teams[0].Epoch != 2 || openedKeys.Teams[0].MembershipGeneration != 5 || !bytes.Equal(openedKeys.Teams[0].Key, bytes.Repeat([]byte{0x62}, 32)) {
		clear(updatedPayload)
		t.Fatal("team key update was not preserved")
	}
	if bytes.Equal(v.Payload, updatedPayload) {
		clear(updatedPayload)
		t.Fatal("team update unexpectedly changed original payload")
	}
	clear(updatedPayload)
}

func TestPasswordVaultRejectsSharingKeyMismatch(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	keys, err := ParseVaultKeys(v.Payload)
	if err != nil {
		t.Fatal(err)
	}
	newSharing, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		keys.Clear()
		t.Fatal(err)
	}
	keys.SharingPrivate = newSharing.Bytes()
	forgedPayload, err := keys.MarshalBinary()
	writer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	keys.Clear()
	if err != nil {
		clear(writer)
		t.Fatal(err)
	}
	defer clear(forgedPayload)
	envelope, body, header := decodeTestEnvelope(t, v.Envelope)
	dataKey := unwrapTestDataKey(t, v.Head, v.Password, v.Envelope)
	defer clear(dataKey)
	aead, err := vaultAEAD(dataKey)
	if err != nil {
		clear(writer)
		t.Fatal(err)
	}
	body.Ciphertext = aead.Seal(nil, header.Nonce, forgedPayload, body.Header)
	bodyBytes, err := encode(body)
	if err != nil {
		clear(writer)
		t.Fatal(err)
	}
	envelope.Body = bodyBytes
	envelope.Signature = ed25519.Sign(writer, vaultSignatureMessage(bodyBytes))
	clear(writer)
	forged, err := encode(envelope)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(forged)
	if _, err := OpenPasswordVault(context.Background(), expectedHead(forged, v.Head), v.Password, forged); !errors.Is(err, ErrVaultUnlock) {
		t.Fatalf("payload sharing key mismatch returned %v", err)
	}
}

func TestPasswordVaultRejectsStaleOrMismatchedHeads(t *testing.T) {
	v := newTestVault(t, true)
	defer clearTestVault(v)
	secondRaw, secondHead := sealTestSuccessor(t, v.Head, v.Protection, v.Payload)
	defer clear(secondRaw)
	cases := []struct {
		name string
		head VaultHead
		raw  []byte
	}{
		{name: "old-head-for-new-document", head: v.Head, raw: secondRaw},
		{name: "new-head-for-old-document", head: secondHead, raw: v.Envelope},
		{name: "wrong-account", head: VaultHead{Issuer: v.Head.Issuer, AccountID: "account_2", Generation: v.Head.Generation, ID: v.Head.ID}, raw: v.Envelope},
		{name: "wrong-issuer", head: VaultHead{Issuer: "https://other.example", AccountID: v.Head.AccountID, Generation: v.Head.Generation, ID: v.Head.ID}, raw: v.Envelope},
		{name: "wrong-document-id", head: VaultHead{Issuer: v.Head.Issuer, AccountID: v.Head.AccountID, Generation: v.Head.Generation, ID: DocumentID{1}}, raw: v.Envelope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OpenPasswordVault(context.Background(), tc.head, v.Password, tc.raw); !errors.Is(err, ErrVaultUnlock) {
				t.Fatalf("stale/mismatched head returned %v", err)
			}
		})
	}
	if _, _, err := SealProtectedVault(context.Background(), v.Head, DocumentID{}, v.Protection, v.Payload); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-zero local head was unexpectedly accepted for a new write: %v", err)
	}
}

func TestPasswordVaultCancellationAndKDFBounds(t *testing.T) {
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := keys.MarshalBinary()
	if err != nil {
		keys.Clear()
		t.Fatal(err)
	}
	defer keys.Clear()
	defer clear(payload)
	head := VaultHead{Issuer: testVaultIssuer, AccountID: testVaultAccount, Generation: 1}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := passwordRoot(canceled, []byte("password"), bytes.Repeat([]byte{1}, 16)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled KDF returned %v", err)
	}
	if _, err := NewVaultProtection(canceled, head, []byte("password"), nil, keys.WriterSeed); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled protection creation returned %v", err)
	}
	if _, _, err := SealPasswordVault(canceled, head, DocumentID{}, []byte("password"), payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled vault seal returned %v", err)
	}
	for name, password := range map[string][]byte{
		"empty":        nil,
		"too-long":     bytes.Repeat([]byte{'a'}, 1025),
		"invalid-utf8": {0xff},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := passwordRoot(context.Background(), password, bytes.Repeat([]byte{2}, 16)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("bounded password returned %v", err)
			}
		})
	}

	vaultKDFSlot <- struct{}{}
	defer func() { <-vaultKDFSlot }()
	waitCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := passwordRoot(waitCtx, []byte("password"), bytes.Repeat([]byte{3}, 16))
		done <- err
	}()
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued KDF returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued KDF did not honor cancellation")
	}
}
