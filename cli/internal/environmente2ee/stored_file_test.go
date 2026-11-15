package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestStoredFileKeyAddressedToExistingMachineKeys(t *testing.T) {
	ctx := context.Background()
	writer, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(signer)
	sign := func(message []byte) []byte { return ed25519.Sign(signer, message) }
	key := bytes.Repeat([]byte{0x71}, 32)
	defer clear(key)
	digest := strings.Repeat("ab", 32)
	first, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, recipient := range []struct {
		machine string
		host    *ecdh.PrivateKey
		other   *ecdh.PrivateKey
	}{{"machine_first", first, second}, {"machine_second", second, first}} {
		t.Run(recipient.machine, func(t *testing.T) {
			c := StoredFileClaims{RequestID: "request_stored", ManifestDigest: digest,
				MachineID: recipient.machine, HostPublic: recipient.host.PublicKey().Bytes(), WriterPublic: writer}
			raw, err := SealStoredFileKey(ctx, c, key, sign)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, key) || len(raw) > 2048 {
				t.Fatal("sealed key leaked plaintext or exceeded its wire limit")
			}
			opened, err := OpenStoredFileKey(ctx, raw, writer, recipient.host.Bytes(), c.RequestID, digest, c.MachineID)
			if err != nil || !bytes.Equal(opened, key) {
				t.Fatal("existing ENV machine key did not open the exact transfer key", err)
			}
			clear(opened)
			for _, wrong := range []struct {
				name, request, digest, machine string
				private, writer                []byte
			}{
				{"request", "request_other", digest, c.MachineID, recipient.host.Bytes(), writer},
				{"manifest", c.RequestID, strings.Repeat("cd", 32), c.MachineID, recipient.host.Bytes(), writer},
				{"machine", c.RequestID, digest, "machine_other", recipient.host.Bytes(), writer},
				{"host-key", c.RequestID, digest, c.MachineID, recipient.other.Bytes(), writer},
				{"sender", c.RequestID, digest, c.MachineID, recipient.host.Bytes(), bytes.Repeat([]byte{9}, 32)},
			} {
				t.Run(wrong.name, func(t *testing.T) {
					got, err := OpenStoredFileKey(ctx, raw, wrong.writer, wrong.private, wrong.request, wrong.digest, wrong.machine)
					clear(got)
					if !errors.Is(err, ErrInvalid) {
						t.Fatal("key opened outside its authenticated context", err)
					}
				})
			}
			tampered := bytes.Clone(raw)
			tampered[len(tampered)-1] ^= 1
			if got, err := OpenStoredFileKey(ctx, tampered, writer, recipient.host.Bytes(), c.RequestID, digest, c.MachineID); err == nil {
				clear(got)
				t.Fatal("tampered signature accepted")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := OpenStoredFileKey(canceled, raw, writer, recipient.host.Bytes(), c.RequestID, digest, c.MachineID); !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation was lost", err)
			}
			if _, err := SealStoredFileKey(canceled, c, key, sign); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled sealing was accepted", err)
			}
			if _, err := SealStoredFileKey(ctx, c, key, func([]byte) []byte { return make([]byte, 64) }); !errors.Is(err, ErrInvalid) {
				t.Fatal("broken signer accepted", err)
			}
		})
	}
}

func TestStoredFileKeyRejectsUnknownSignedClaims(t *testing.T) {
	writer, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(signer)
	host, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	// Even a valid sender signature cannot add a restriction this client does
	// not understand. Signed authorization claims remain an exact array.
	claims, err := encode([]any{storedFileKeyDomain, uint64(1), "request_stored", digest,
		"machine_first", host.PublicKey().Bytes(), writer, "unknown-restriction"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sealVaultDelivery(context.Background(), storedFileKeyDomain, claims, host.PublicKey().Bytes(), signer.Seed(), bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if key, err := OpenStoredFileKey(context.Background(), raw, writer, host.Bytes(), "request_stored", digest, "machine_first"); !errors.Is(err, ErrInvalid) {
		clear(key)
		t.Fatal("unknown signed claim accepted", err)
	}
}
