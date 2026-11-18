package recording

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"

	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// Rotation is generation N+1 of a team's recording key, made by a member who holds generation N. Only the public
// halves, the signature and the sealed previous bundle go to the server; Bundle is the new private half, for
// sealing the new generation to members, and the caller clears it.
type Rotation struct {
	PublicKey         []byte // X25519
	SigningPublicKey  []byte // Ed25519
	RotationSignature []byte // by generation N's signing key: the only thing a machine accepts the new key for
	SealedPrevious    []byte // generation N's bundle sealed to PublicKey, the link that keeps older recordings readable
	Bundle            Bundle
}

// Rotate makes generation current+1. A machine follows it only because the signature verifies under the signing
// key of the generation it pinned (wire.VerifyRotation), never because the server says so.
func Rotate(teamID string, current int64, old Bundle) (Rotation, error) {
	if teamID == "" || current < 1 || len(old.Private) != 32 || len(old.Signing) != ed25519.PrivateKeySize {
		return Rotation{}, ErrInvalid
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Rotation{}, err
	}
	signingPublic, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Rotation{}, err
	}
	public := private.PublicKey().Bytes()
	previous, err := SealBundle(public, teamID, current, old)
	if err != nil {
		clear(signing)
		return Rotation{}, err
	}
	signature := ed25519.Sign(old.Signing, wire.RotationMessage(teamID, current+1, public, signingPublic))
	return Rotation{PublicKey: public, SigningPublicKey: signingPublic, RotationSignature: signature, SealedPrevious: previous, Bundle: Bundle{Private: private.Bytes(), Signing: signing}}, nil
}

// ErrNoCopy says this account cannot replace the key: it holds no copy of the current generation, and only a holder's
// signing key can sign the next one.
var ErrNoCopy = errors.New("this account holds no copy of the current team recording key")

// ReplaceKey is the whole client side of replacing a team's recording key, for the CLI (the console does the same in
// its vault worker): open this account's copy of the current generation with its vault sharing key, make the next
// generation signed by it, seal the old bundle to the new key and the new bundle to every viewer with a vault. The
// result is the body of POST /v1/teams/{team}/recording-key; the server only checks the signature and stores it.
func ReplaceKey(sharingPrivate []byte, teamID string, state wire.KeyState) (wire.KeyCreate, error) {
	if state.Key == nil || state.MyKey == nil || state.MyKey.Generation != state.Key.Generation {
		return wire.KeyCreate{}, ErrNoCopy
	}
	var viewers []wire.Viewer
	for _, viewer := range state.Viewers {
		if viewer.AccountID != "" && len(viewer.SharingPublicKey) > 0 {
			viewers = append(viewers, viewer)
		}
	}
	if len(viewers) == 0 {
		return wire.KeyCreate{}, ErrInvalid
	}
	old, err := OpenBundle(sharingPrivate, teamID, state.MyKey.Generation, state.MyKey.Sealed)
	if err != nil {
		return wire.KeyCreate{}, err
	}
	defer clear(old.Private)
	defer clear(old.Signing)
	rotation, err := Rotate(teamID, state.Key.Generation, old)
	if err != nil {
		return wire.KeyCreate{}, err
	}
	defer clear(rotation.Bundle.Private)
	defer clear(rotation.Bundle.Signing)
	grants := make([]wire.Grant, 0, len(viewers))
	for _, viewer := range viewers {
		sealed, err := SealBundle(viewer.SharingPublicKey, teamID, state.Key.Generation+1, rotation.Bundle)
		if err != nil {
			return wire.KeyCreate{}, err
		}
		grants = append(grants, wire.Grant{AccountID: viewer.AccountID, SharingPublicKey: viewer.SharingPublicKey, Sealed: sealed})
	}
	return wire.KeyCreate{
		Key:            wire.Key{Generation: state.Key.Generation + 1, PublicKey: rotation.PublicKey, SigningPublicKey: rotation.SigningPublicKey, RotationSignature: rotation.RotationSignature},
		SealedPrevious: rotation.SealedPrevious,
		Grants:         grants,
	}, nil
}
