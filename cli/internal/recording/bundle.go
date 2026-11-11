package recording

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"strconv"
)

// Bundle is one generation of the team recording key: the X25519 private key that opens recordings'
// data keys and the Ed25519 key that signs the next generation.
type Bundle struct {
	Private []byte // X25519, 32 bytes
	Signing []byte // Ed25519 private key, 64 bytes
}

const bundleBytes = 32 + ed25519.PrivateKeySize

func bundleContext(teamID string, generation int64) (info, aad []byte) {
	return []byte("paperboat recording key bundle v1"), []byte(teamID + "\x00" + strconv.FormatInt(generation, 10))
}

// SealBundle seals generation's bundle to recipient: a viewer's vault sharing public key (a viewer's
// copy), or the next generation's recording public key (the chain link that opens older recordings).
// The result is the HPKE encapsulated key (32 bytes) followed by the ciphertext.
func SealBundle(recipient []byte, teamID string, generation int64, bundle Bundle) ([]byte, error) {
	if len(bundle.Private) != 32 || len(bundle.Signing) != ed25519.PrivateKeySize || teamID == "" || generation < 1 {
		return nil, ErrInvalid
	}
	public, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(recipient)
	if err != nil {
		return nil, ErrInvalid
	}
	info, aad := bundleContext(teamID, generation)
	enc, sender, err := hpke.NewSender(public, hpke.HKDFSHA256(), hpke.AES256GCM(), info)
	if err != nil {
		return nil, ErrInvalid
	}
	plaintext := append(append(make([]byte, 0, bundleBytes), bundle.Private...), bundle.Signing...)
	defer clear(plaintext)
	ciphertext, err := sender.Seal(aad, plaintext)
	if err != nil {
		return nil, ErrInvalid
	}
	return append(enc, ciphertext...), nil
}

// OpenBundle opens a sealed bundle with the recipient's X25519 private key.
func OpenBundle(recipientPrivate []byte, teamID string, generation int64, sealed []byte) (Bundle, error) {
	if len(sealed) < 32 || len(sealed) > 4096 {
		return Bundle{}, ErrInvalid
	}
	private, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(recipientPrivate)
	if err != nil {
		return Bundle{}, ErrInvalid
	}
	info, aad := bundleContext(teamID, generation)
	recipient, err := hpke.NewRecipient(sealed[:32], private, hpke.HKDFSHA256(), hpke.AES256GCM(), info)
	if err != nil {
		return Bundle{}, ErrInvalid
	}
	plaintext, err := recipient.Open(aad, sealed[32:])
	if err != nil || len(plaintext) != bundleBytes {
		return Bundle{}, ErrInvalid
	}
	return Bundle{Private: plaintext[:32:32], Signing: plaintext[32:]}, nil
}

// ChainLink is one rotation's sealed previous bundle, as the replay answer lists it.
type ChainLink struct {
	Generation     int64  // the generation whose public key it is sealed to
	SealedPrevious []byte // generation-1's bundle
}

// OpenRecordingKey returns the X25519 private key of generation want: the viewer's copy of
// generation have opened with the vault sharing key, then each chain link down to want.
func OpenRecordingKey(sharingPrivate []byte, teamID string, have int64, sealed []byte, chain []ChainLink, want int64) ([]byte, error) {
	bundle, err := OpenBundle(sharingPrivate, teamID, have, sealed)
	if err != nil || want < 1 || want > have {
		return nil, ErrInvalid
	}
	for generation := have; generation > want; generation-- {
		var link *ChainLink
		for i := range chain {
			if chain[i].Generation == generation {
				link = &chain[i]
			}
		}
		if link == nil {
			clear(bundle.Signing)
			return nil, ErrInvalid
		}
		previous, err := OpenBundle(bundle.Private, teamID, generation-1, link.SealedPrevious)
		clear(bundle.Private)
		clear(bundle.Signing)
		if err != nil {
			return nil, err
		}
		bundle = previous
	}
	clear(bundle.Signing)
	return bundle.Private, nil
}
