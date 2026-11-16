package recipekey

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// After a rotate, the recipe's new key sealed to this machine replaces the old one in the key file, so
// a running machine keeps opening its vaults without pairing again (F26).
func TestRotatedReplacesTheRecipeKey(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	old, _ := recipe.NewToken("")
	f, err := New(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(config, f); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := Load(config)
	if err != nil || !ok || !bytes.Equal(loaded.Machine, f.Machine) {
		t.Fatalf("load %v %v", ok, err)
	}
	next, _ := recipe.NewToken(old.ID)
	public, _ := loaded.MachinePublic()
	sealed, err := recipe.SealRecipeKey(public, old.ID, next.PrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	got, changed, err := Rotated(config, loaded, sealed)
	if err != nil || !changed || !bytes.Equal(got.PrivateKey, next.PrivateKey()) {
		t.Fatalf("rotated %v %v", changed, err)
	}
	again, _, _ := Load(config)
	if !bytes.Equal(again.PrivateKey, next.PrivateKey()) {
		t.Fatal("the new key was not saved")
	}
	if _, changed, err := Rotated(config, again, sealed); err != nil || changed {
		t.Fatalf("a second delivery changed the file: %v %v", changed, err)
	}
	other, _ := New(old)
	if _, _, err := Rotated(config, other, sealed); err == nil {
		t.Fatal("another machine opened the sealed key")
	}
	if _, ok, err := Load(filepath.Join(t.TempDir(), "config.json")); ok || err != nil {
		t.Fatalf("no file: %v %v", ok, err)
	}
}
