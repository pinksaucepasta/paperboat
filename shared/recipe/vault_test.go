package recipe

import (
	"bytes"
	"testing"
)

// A vault key wrapped to a recipe opens only with that recipe's token, for that vault; values sealed
// under the key open only for the same vault and key epoch (F26-04).
func TestVaultKeyWrapsAndValues(t *testing.T) {
	token, _ := NewToken("")
	other, _ := NewToken("")
	key, err := NewVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	info := RecipientInfo("canary-env", "recipe:recipe_1")
	wrapped, err := WrapToPublic(token.PublicKey(), info, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := UnwrapWithPrivate(token.PrivateKey(), info, wrapped); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unwrap: %v", err)
	}
	if _, err := UnwrapWithPrivate(other.PrivateKey(), info, wrapped); err == nil {
		t.Fatal("another recipe opened the wrap")
	}
	if _, err := UnwrapWithPrivate(token.PrivateKey(), RecipientInfo("prod-env", "recipe:recipe_1"), wrapped); err == nil {
		t.Fatal("the wrap opened for another vault")
	}

	teamKey, _ := NewVaultKey()
	tw, err := WrapToTeam(teamKey, "canary-env", "acme", 3, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := UnwrapWithTeam(teamKey, "canary-env", "acme", 3, tw); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("team unwrap: %v", err)
	}
	if _, err := UnwrapWithTeam(teamKey, "canary-env", "acme", 4, tw); err == nil {
		t.Fatal("a team wrap opened under another epoch")
	}

	envelope, err := SealValues(key, "canary-env", 1, map[string]string{"API_KEY": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := OpenValues(key, "canary-env", 1, envelope); err != nil || v["API_KEY"] != "s3cret" {
		t.Fatalf("values: %v %v", v, err)
	}
	if _, err := OpenValues(key, "canary-env", 2, envelope); err == nil {
		t.Fatal("values opened under another key epoch")
	}
	if v, err := OpenValues(key, "canary-env", 1, nil); err != nil || len(v) != 0 {
		t.Fatalf("empty envelope: %v %v", v, err)
	}
}
