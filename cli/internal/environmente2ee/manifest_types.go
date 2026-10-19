package environmente2ee

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"io"
)

type Recipient struct {
	Kind          RecipientKind
	SubjectID     string
	KeyGeneration uint64
	KeyID         string
	PublicKey     []byte
}

type RecipientPrivate struct {
	Kind          RecipientKind
	SubjectID     string
	KeyGeneration uint64
	KeyID         string
	PrivateKey    []byte
}

type RecipientWrap struct {
	Kind            RecipientKind
	SubjectID       string
	KeyGeneration   uint64
	KeyID           string
	EncapsulatedKey []byte
	Ciphertext      []byte
}

type ManifestClaims struct {
	AccountID           string
	AuthorityGeneration uint64
	AuthorityID         DocumentID
	Scope               ScopeKind
	MachineID           string
	State               ScopeState
	PreviousVersion     uint64
	Version             uint64
	KeyEpoch            uint64
	OperationID         [16]byte
	Salt                []byte
	Nonce               []byte
	Mutation            MutationKind
	ChangedNames        []string
	Names               []string
	CiphertextSHA256    DocumentID
	Ciphertext          []byte
	Wraps               []RecipientWrap
}

type Manifest struct {
	ManifestClaims
	SignerKeyID string
	ID          DocumentID
	Raw         []byte
}

type BuildManifestInput struct {
	Claims        ManifestClaims
	Values        map[string][]byte
	ScopeKey      []byte
	Recipients    []Recipient
	SignerKeyID   string
	SignerPrivate ed25519.PrivateKey
	Random        io.Reader
}

type DecryptedScope struct {
	Values   map[string][]byte
	ScopeKey []byte
}

type FirstHostDelivery struct {
	Global    DecryptedScope
	Machine   DecryptedScope
	Effective map[string][]byte
}

func GenerateRecipientKey() (private, public []byte, err error) {
	key, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		return nil, nil, err
	}
	private, err = key.Bytes()
	if err != nil {
		return nil, nil, err
	}
	return private, key.PublicKey().Bytes(), nil
}
