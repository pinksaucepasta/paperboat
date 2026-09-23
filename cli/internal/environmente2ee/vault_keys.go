package environmente2ee

import (
	"crypto/ecdh"
	"crypto/rand"
)

// MaximumVaultTeams bounds the key inventory of a single user to 128 teams.
// It bounds decoding independently of the encrypted envelope byte limit.
const MaximumVaultTeams = 128

type VaultTeamKey struct {
	_                    struct{} `cbor:",toarray"`
	TeamID               string
	Epoch                uint64
	MembershipGeneration uint64
	Key                  []byte
}

// VaultKeys contains only ENV keys. Device signing and transport private keys
// never enter this structure or the server-stored password envelope.
type VaultKeys struct {
	_                 struct{} `cbor:",toarray"`
	Domain            string
	Version           uint64
	PersonalEpoch     uint64
	PersonalKey       []byte
	SharingGeneration uint64
	SharingPrivate    []byte
	Teams             []VaultTeamKey
	WriterSeed        []byte
}

// The wire type has no BinaryMarshaler methods: encoding VaultKeys directly
// would recursively invoke MarshalBinary through the CBOR library.
type vaultKeysWire VaultKeys

func NewVaultKeys() (VaultKeys, error) {
	sharing, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return VaultKeys{}, err
	}
	keys := VaultKeys{Domain: "paperboat.environment.vault-keys", Version: 1, PersonalEpoch: 1, PersonalKey: make([]byte, 32), SharingGeneration: 1, SharingPrivate: sharing.Bytes(), Teams: []VaultTeamKey{}, WriterSeed: make([]byte, 32)}
	if _, err := rand.Read(keys.PersonalKey); err != nil {
		keys.Clear()
		return VaultKeys{}, err
	}
	if _, err := rand.Read(keys.WriterSeed); err != nil {
		keys.Clear()
		return VaultKeys{}, err
	}
	return keys, nil
}

func (k *VaultKeys) Clear() {
	clear(k.WriterSeed)
	clear(k.PersonalKey)
	clear(k.SharingPrivate)
	for i := range k.Teams {
		clear(k.Teams[i].Key)
	}
	*k = VaultKeys{}
}

func (k VaultKeys) valid() bool {
	if k.Domain != "paperboat.environment.vault-keys" || k.Version != 1 ||
		k.PersonalEpoch == 0 || k.PersonalEpoch > MaximumContractInteger || len(k.PersonalKey) != 32 || allZero(k.PersonalKey) ||
		k.SharingGeneration == 0 || k.SharingGeneration > MaximumContractInteger || len(k.SharingPrivate) != 32 || allZero(k.SharingPrivate) ||
		len(k.WriterSeed) != 32 || allZero(k.WriterSeed) || k.Teams == nil || len(k.Teams) > MaximumVaultTeams {
		return false
	}
	previous := ""
	for _, team := range k.Teams {
		if !validIdentifier(team.TeamID) || team.TeamID <= previous || team.Epoch == 0 || team.Epoch > MaximumContractInteger || team.MembershipGeneration == 0 || team.MembershipGeneration > MaximumContractInteger || len(team.Key) != 32 || allZero(team.Key) {
			return false
		}
		previous = team.TeamID
	}
	return true
}

func (k VaultKeys) MarshalBinary() ([]byte, error) {
	if !k.valid() {
		return nil, ErrInvalid
	}
	return encode(vaultKeysWire(k))
}

func ParseVaultKeys(raw []byte) (VaultKeys, error) {
	var wire vaultKeysWire
	err := decodeCanonical(raw, MaximumScopeBytes, &wire)
	keys := VaultKeys(wire)
	if err != nil || !keys.valid() {
		keys.Clear()
		return VaultKeys{}, ErrInvalid
	}
	return keys, nil
}
