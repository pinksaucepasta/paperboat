// Package recipekey is a recipe machine's key file (PLAN F26): the recipe's token id and X25519
// private key (never the token's secret) and the machine's own X25519 key, owner-only next to the
// pb config. pb pair writes it; pb env run and the daemon read it; a rotate replaces the recipe key.
package recipekey

import (
	"bytes"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// File is the key file. Machine is the machine's own private key; rotate seals the recipe's new key
// to its public half.
type File struct {
	TokenID    string `json:"token_id"`
	PrivateKey []byte `json:"private_key"`
	Machine    []byte `json:"machine_key"`
}

// Path is the key file of the pb config at configPath.
func Path(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "recipe-key.json")
}

// New is the key file for a token, with a fresh machine key.
func New(token recipe.Token) (File, error) {
	machine, err := recipe.NewVaultKey() // any 32 random bytes are an X25519 private key
	if err != nil {
		return File{}, err
	}
	return File{TokenID: token.ID, PrivateKey: token.PrivateKey(), Machine: machine}, nil
}

// MachinePublic is the public half of the machine key.
func (f File) MachinePublic() ([]byte, error) {
	key, err := ecdh.X25519().NewPrivateKey(f.Machine)
	if err != nil {
		return nil, err
	}
	return key.PublicKey().Bytes(), nil
}

// Save writes the file atomically, owner-only.
func Save(configPath string, f File) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	path := Path(configPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("save the recipe key: %w", err)
	}
	return os.Rename(temporary, path)
}

// Load reads the file; ok is false on a machine not paired from a recipe.
func Load(configPath string) (File, bool, error) {
	data, err := os.ReadFile(Path(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil || !recipe.ValidID(f.TokenID) || len(f.PrivateKey) != 32 || len(f.Machine) != 32 {
		return File{}, false, errors.New("the recipe key file is damaged; run the recipe install again")
	}
	return f, true, nil
}

// Rotated opens the recipe's new key sealed to this machine after a rotate and saves it; changed is
// false when the file already holds it.
func Rotated(configPath string, f File, sealed []byte) (File, bool, error) {
	key, err := recipe.OpenRecipeKey(f.Machine, f.TokenID, sealed)
	if err != nil {
		return f, false, fmt.Errorf("open the recipe's new key sent after rotate: %w", err)
	}
	if bytes.Equal(key, f.PrivateKey) {
		return f, false, nil
	}
	f.PrivateKey = key
	return f, true, Save(configPath, f)
}
