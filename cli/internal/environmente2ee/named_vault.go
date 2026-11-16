package environmente2ee

import (
	"crypto/ecdh"
	"errors"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// Named ENV vaults (PLAN F26): a vault key is wrapped with this account's unlocked ENV keys, to its
// X25519 sharing key ("account") and, for a team vault, to the team's ENV key ("team"). The CLI and
// the browser engine (cmd/environmentvault-wasm) both use these, so a wrap made in one opens in the
// other. The byte formats are shared/recipe's (vault.go).

// NamedVaultWrap is one recipient's wrap of a vault key, as the server stores it.
type NamedVaultWrap struct {
	Kind    string `json:"recipient_kind"`
	ID      string `json:"recipient_id"`
	Wrapped []byte `json:"wrapped"`
}

// ErrNamedVaultNoWrap means none of the vault's wraps opens with this account's keys (a team vault whose
// team key this computer has not received, or a wrap made before a key rotation).
var ErrNamedVaultNoWrap = errors.New("this account's ENV keys do not open the vault key")

func sharingPublic(keys *VaultKeys) ([]byte, error) {
	private, err := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
	if err != nil {
		return nil, ErrInvalid
	}
	return private.PublicKey().Bytes(), nil
}

// NamedVaultWraps wraps a vault key for account and, when team is not empty, for that team.
func NamedVaultWraps(keys *VaultKeys, account, team, vault string, key []byte) ([]NamedVaultWrap, error) {
	public, err := sharingPublic(keys)
	if err != nil {
		return nil, err
	}
	own, err := recipe.WrapToPublic(public, recipe.RecipientInfo(vault, "account:"+account), key)
	if err != nil {
		return nil, err
	}
	out := []NamedVaultWrap{{Kind: "account", ID: account, Wrapped: own}}
	if team == "" {
		return out, nil
	}
	for _, t := range keys.Teams {
		if t.TeamID == team {
			wrapped, err := recipe.WrapToTeam(t.Key, vault, team, t.Epoch, key)
			if err != nil {
				return nil, err
			}
			return append(out, NamedVaultWrap{Kind: "team", ID: team, Wrapped: wrapped}), nil
		}
	}
	return nil, ErrVaultTeamKeyMissing
}

// ErrVaultTeamKeyMissing means this computer has not received the team's ENV key (pb env grants sync).
var ErrVaultTeamKeyMissing = errors.New("this computer has no ENV key for the team")

// OpenNamedVaultKey opens the vault key from the first wrap this account's keys open.
func OpenNamedVaultKey(keys *VaultKeys, account, vault string, wraps []NamedVaultWrap) ([]byte, error) {
	for _, w := range wraps {
		switch w.Kind {
		case "account":
			if w.ID != account {
				continue
			}
			if key, err := recipe.UnwrapWithPrivate(keys.SharingPrivate, recipe.RecipientInfo(vault, "account:"+account), w.Wrapped); err == nil {
				return key, nil
			}
		case "team":
			for _, t := range keys.Teams {
				if t.TeamID == w.ID {
					if key, err := recipe.UnwrapWithTeam(t.Key, vault, t.TeamID, t.Epoch, w.Wrapped); err == nil {
						return key, nil
					}
				}
			}
		}
	}
	return nil, ErrNamedVaultNoWrap
}

// WrapNamedVaultForRecipe opens a vault's key with this account's keys and wraps it to a recipe's
// public key, for recipe create and update (the recipe's machines unwrap it with the token).
func WrapNamedVaultForRecipe(keys *VaultKeys, account, vault string, wraps []NamedVaultWrap, recipePublic []byte, tokenID string) ([]byte, error) {
	key, err := OpenNamedVaultKey(keys, account, vault, wraps)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return recipe.WrapToPublic(recipePublic, recipe.RecipeVaultInfo(vault, tokenID), key)
}
