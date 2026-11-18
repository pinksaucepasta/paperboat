package secrets

import (
	"bytes"
	"errors"
	"testing"
)

func TestByteEnvelopeRoundTripAndTamper(t *testing.T) {
	key := []byte("paperboat-test-envelope-key-0123456789")
	plaintext := []byte("sensitive certificate private key")
	wantKey := append([]byte(nil), key...)
	wantPlaintext := append([]byte(nil), plaintext...)

	ciphertext, err := EncryptBytes(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ciphertext, plaintext) || !bytes.Equal(key, wantKey) || !bytes.Equal(plaintext, wantPlaintext) {
		t.Fatal("byte envelope exposed or modified caller-owned input")
	}
	opened, err := DecryptBytes(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q", opened)
	}
	clear(opened)

	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	if _, err := DecryptBytes(key, tampered); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered ciphertext error = %v", err)
	}
}

func TestByteEnvelopeRejectsEmptyPlaintext(t *testing.T) {
	if _, err := EncryptBytes([]byte("paperboat-test-envelope-key-0123456789"), nil); err == nil {
		t.Fatal("empty plaintext was accepted")
	}
}

func TestLegacyEnvelopeCompatibility(t *testing.T) {
	const key = "paperboat-test-envelope-key-0123456789"
	const plaintext = "legacy secret"
	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if opened != plaintext {
		t.Fatalf("opened plaintext = %q", opened)
	}
}

func TestKeyringRotationOpensOldAndNewAndResealsToTheNewest(t *testing.T) {
	const old, next = "old-key-0123456789-0123456789-0123456789", "new-key-0123456789-0123456789-0123456789"
	sealedOld, err := Encrypt(old, "seed")
	if err != nil {
		t.Fatal(err)
	}
	ring := next + "," + old
	if got, err := Decrypt(ring, sealedOld); err != nil || got != "seed" {
		t.Fatalf("new ring must open a ciphertext of the old key: %q %v", got, err)
	}
	if !NeedsReseal(ring, sealedOld) {
		t.Fatal("a ciphertext of the old key must need resealing")
	}
	resealed, err := Reseal(ring, sealedOld)
	if err != nil || NeedsReseal(ring, resealed) {
		t.Fatalf("resealed ciphertext must carry the newest key id: %v", err)
	}
	if got, err := Decrypt(next, resealed); err != nil || got != "seed" {
		t.Fatalf("the new key alone must open the resealed ciphertext: %q %v", got, err)
	}
	if _, err := Decrypt(old, resealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("the old key must not open a resealed ciphertext: %v", err)
	}
	if ids := KeyIDs(ring); len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("key ids = %v", ids)
	}
}

func TestCiphertextWithoutAStampIsOpenedByAnyKeyOfTheRing(t *testing.T) {
	const key = "unstamped-key-0123456789-0123456789-0123"
	gcm, err := gcmFor([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{7}, gcm.NonceSize())
	raw := append(append([]byte(nil), nonce...), gcm.Seal(nil, nonce, []byte("plain"), nil)...)
	if got, err := Decrypt("other-key-0123456789-0123456789-0123,"+key, raw); err != nil || got != "plain" {
		t.Fatalf("got %q %v", got, err)
	}
	if !NeedsReseal(key, raw) {
		t.Fatal("an unstamped ciphertext must need resealing")
	}
}

func TestValidRingRefusesEmptyKeys(t *testing.T) {
	for ring, want := range map[string]bool{"a": true, "new,old": true, "": false, ",old": false, "new,": false, "new,,old": false} {
		if got := ValidRing(ring); got != want {
			t.Errorf("ValidRing(%q) = %v, want %v", ring, got, want)
		}
	}
}
