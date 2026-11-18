package recipe

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/shared/envname"
)

// Named ENV vault crypto (PLAN F26 section 6), one implementation for the CLI, the daemon and the
// browser (cli/cmd/environmentvault-wasm). A vault has a random 32-byte key. The key is wrapped:
//   - to an X25519 public key (a recipe's PublicKey, an account's ENV sharing key) with HPKE base
//     mode, DHKEM(X25519)/HKDF-SHA256/AES-256-GCM, single shot: enc (32) || ciphertext (48);
//   - to a team's symmetric ENV key with AES-256-GCM: nonce (12) || ciphertext (48).
// The values are one JSON object {name: value} sealed with AES-256-GCM under the vault key:
// nonce (12) || ciphertext. Every seal binds its purpose, the vault name and the recipient or key
// epoch as HPKE info or AEAD additional data, so a wrap or envelope cannot be moved to another vault.

// VaultKeySize is the size of a vault key.
const VaultKeySize = 32

// ErrVaultCrypto is a wrap or envelope that does not open with the given key.
var ErrVaultCrypto = errors.New("the vault key or values do not open with this key")

func vaultSuite() (hpke.KDF, hpke.AEAD) { return hpke.HKDFSHA256(), hpke.AES256GCM() }

// RecipientInfo binds a wrap to its vault and recipient ("recipe:<id>", "account:<id>").
func RecipientInfo(vault, recipient string) []byte {
	return []byte("paperboat vault key v1\x00" + vault + "\x00" + recipient)
}

// RecipeVaultInfo binds a recipe's vault wrap to the vault and the recipe's token id (kept by rotate).
func RecipeVaultInfo(vault, tokenID string) []byte { return RecipientInfo(vault, "recipe:"+tokenID) }

// NewVaultKey returns a fresh vault key.
func NewVaultKey() ([]byte, error) {
	key := make([]byte, VaultKeySize)
	_, err := rand.Read(key)
	return key, err
}

// WrapToPublic wraps a vault key to an X25519 public key.
func WrapToPublic(public []byte, info []byte, vaultKey []byte) ([]byte, error) {
	if len(vaultKey) != VaultKeySize {
		return nil, ErrVaultCrypto
	}
	pk, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(public)
	if err != nil {
		return nil, fmt.Errorf("wrap the vault key: %w", err)
	}
	kdf, aead := vaultSuite()
	return hpke.Seal(pk, kdf, aead, info, vaultKey)
}

// UnwrapWithPrivate opens a WrapToPublic wrap with the X25519 private key.
func UnwrapWithPrivate(private []byte, info []byte, wrapped []byte) ([]byte, error) {
	sk, err := hpke.DHKEM(ecdh.X25519()).NewPrivateKey(private)
	if err != nil {
		return nil, ErrVaultCrypto
	}
	kdf, aead := vaultSuite()
	key, err := hpke.Open(sk, kdf, aead, info, wrapped)
	if err != nil || len(key) != VaultKeySize {
		return nil, ErrVaultCrypto
	}
	return key, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrVaultCrypto
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(key, aad, plaintext []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func open(key, aad, sealed []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, ErrVaultCrypto
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrVaultCrypto
	}
	return plain, nil
}

func teamAAD(vault, team string, epoch uint64) []byte {
	return []byte(fmt.Sprintf("paperboat vault key v1\x00%s\x00team:%s\x00%d", vault, team, epoch))
}

// WrapToTeam wraps a vault key with a team's symmetric ENV key of the given epoch.
func WrapToTeam(teamKey []byte, vault, team string, epoch uint64, vaultKey []byte) ([]byte, error) {
	if len(vaultKey) != VaultKeySize {
		return nil, ErrVaultCrypto
	}
	return seal(teamKey, teamAAD(vault, team, epoch), vaultKey)
}

// UnwrapWithTeam opens a WrapToTeam wrap.
func UnwrapWithTeam(teamKey []byte, vault, team string, epoch uint64, wrapped []byte) ([]byte, error) {
	key, err := open(teamKey, teamAAD(vault, team, epoch), wrapped)
	if err != nil || len(key) != VaultKeySize {
		return nil, ErrVaultCrypto
	}
	return key, nil
}

func valuesAAD(vault string, keyEpoch int) []byte {
	return []byte(fmt.Sprintf("paperboat vault values v1\x00%s\x00%d", vault, keyEpoch))
}

// SealValues seals a vault's values under its key of the given epoch.
func SealValues(vaultKey []byte, vault string, keyEpoch int, values map[string]string) ([]byte, error) {
	if values == nil {
		values = map[string]string{}
	}
	// The server only sees the sealed envelope, so this is where a vault's names are checked.
	for name := range values {
		if err := envname.Check(name); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	plain, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return seal(vaultKey, valuesAAD(vault, keyEpoch), plain)
}

// OpenValues opens a SealValues envelope; an empty envelope is a vault with no values yet.
func OpenValues(vaultKey []byte, vault string, keyEpoch int, envelope []byte) (map[string]string, error) {
	values := map[string]string{}
	if len(envelope) == 0 {
		return values, nil
	}
	plain, err := open(vaultKey, valuesAAD(vault, keyEpoch), envelope)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, ErrVaultCrypto
	}
	return values, nil
}

// SealRecipeKey seals a recipe's new private key (after rotate) to one of its machines' own X25519
// public key, so the machine keeps opening the recipe's vaults; the server only stores the result.
func SealRecipeKey(machinePublic []byte, tokenID string, recipePrivate []byte) ([]byte, error) {
	return WrapToPublic(machinePublic, RecipientInfo("recipe-key", "recipe:"+tokenID), recipePrivate)
}

// OpenRecipeKey opens a SealRecipeKey result with the machine's private key.
func OpenRecipeKey(machinePrivate []byte, tokenID string, sealed []byte) ([]byte, error) {
	return UnwrapWithPrivate(machinePrivate, RecipientInfo("recipe-key", "recipe:"+tokenID), sealed)
}
