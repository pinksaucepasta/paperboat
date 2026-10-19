package connectorrotation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// KeyReference is safe to persist. PublicKey is public identity material,
// never a private key. Reference must point at a platform credential store.
type KeyReference struct {
	Reference  string `json:"reference"`
	KeyID      string `json:"key_id"`
	Thumbprint string `json:"thumbprint"`
	PublicKey  []byte `json:"public_key"`
}

func (r KeyReference) Validate() error {
	if err := connectorprotocol.ValidateCredentialReference(r.Reference); err != nil ||
		connectorprotocol.ValidateIdentityKey(r.KeyID, r.Thumbprint) != nil ||
		len(r.PublicKey) != ed25519.PublicKeySize {
		return ErrInvalidConfig
	}
	thumbprint, err := connectorprotocol.IdentityThumbprint(ed25519.PublicKey(r.PublicKey))
	if err != nil || thumbprint != r.Thumbprint || r.KeyID != "ed25519:"+thumbprint {
		return ErrInvalidConfig
	}
	return nil
}

func (r KeyReference) clone() KeyReference {
	r.PublicKey = append([]byte(nil), r.PublicKey...)
	return r
}

// KeyStore is deliberately reference-oriented. Put receives a newly-created
// private key only so the implementation can place it into Keychain,
// Credential Manager, Secret Service, TPM-backed storage, or an equivalent
// protected facility. Callers can never retrieve private bytes through this
// interface. Sign also keeps key material inside that facility.
type KeyStore interface {
	Put(context.Context, ed25519.PrivateKey) (KeyReference, error)
	Sign(context.Context, string, []byte) ([]byte, error)
	// Delete must be idempotent. A missing reference is success because a
	// process can crash after deletion and before its terminal journal save.
	Delete(context.Context, string) error
}

// GenerateReplacementKey creates a fresh Ed25519 key using the supplied
// cryptographically secure reader and immediately transfers the private key
// to the injected platform store. The private slice is cleared before return.
func GenerateReplacementKey(ctx context.Context, store KeyStore, random io.Reader) (KeyReference, error) {
	if ctx == nil || store == nil {
		return KeyReference{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return KeyReference{}, err
	}
	if random == nil {
		random = rand.Reader
	}
	public, private, err := ed25519.GenerateKey(random)
	if err != nil {
		return KeyReference{}, err
	}
	defer clear(private)

	thumbprint, err := connectorprotocol.IdentityThumbprint(public)
	if err != nil {
		return KeyReference{}, err
	}
	want := KeyReference{
		KeyID:      "ed25519:" + thumbprint,
		Thumbprint: thumbprint,
		PublicKey:  append([]byte(nil), public...),
	}
	got, err := store.Put(ctx, private)
	if err != nil {
		return KeyReference{}, safeRotationFailure("replacement credential could not be stored", err)
	}
	if err := ctx.Err(); err != nil {
		var cleanupErr error
		if got.Reference != "" {
			cleanupErr = store.Delete(context.Background(), got.Reference)
		}
		return KeyReference{}, safeRotationFailure("replacement credential setup was canceled", errors.Join(err, cleanupErr))
	}
	if err := got.Validate(); err != nil || got.KeyID != want.KeyID || got.Thumbprint != want.Thumbprint || !equalBytes(got.PublicKey, want.PublicKey) {
		var cleanupErr error
		if got.Reference != "" {
			cleanupErr = store.Delete(context.Background(), got.Reference)
		}
		return KeyReference{}, safeRotationFailure("replacement credential store returned an invalid key", errors.Join(ErrKeyUnavailable, err, cleanupErr))
	}
	return got.clone(), nil
}

// Installer applies a replacement credential to the host's replacement
// connector process and later stops the old process/session. Both methods
// must be idempotent for the exact operation and generation. No private key is
// passed: the runtime resolves NewCredentialReference through KeyStore.
type Installer interface {
	Install(context.Context, connectorprotocol.CredentialRotationInstall) error
	Revoke(context.Context, connectorprotocol.CredentialRotationRevoke) error
}

// Journal is the durable metadata boundary for one or more rotations. A
// journal stores references and protocol metadata only. Implementations must
// make Save idempotent for the same operation and reject identity changes.
type Journal interface {
	Load(context.Context, string) (Record, error)
	Save(context.Context, Record) error
}
