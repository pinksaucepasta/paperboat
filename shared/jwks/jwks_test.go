package jwks

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func pub(t *testing.T) string {
	t.Helper()
	p, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(p)
}

func key(t *testing.T, kid string) string {
	return `{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"` + kid + `","x":"` + pub(t) + `"}`
}

func TestParseHonorsWithdrawnKeysAndBounds(t *testing.T) {
	both := `{"keys":[` + key(t, "old") + `,` + key(t, "new") + `]`
	keys, err := Parse([]byte(both + `}`))
	if err != nil || len(keys) != 2 {
		t.Fatalf("overlap = %v, %v", keys, err)
	}
	keys, err = Parse([]byte(both + `,"withdrawn_key_ids":["old","never-listed"]}`))
	if err != nil || len(keys) != 1 || keys["new"] == nil {
		t.Fatalf("withdrawn old kept: %v, %v", keys, err)
	}
	if _, err = Parse([]byte(both + `,"withdrawn_key_ids":["old","new"]}`)); err == nil {
		t.Fatal("a document that withdraws every key was accepted")
	}
	many := make([]string, MaxKeys+1)
	for i := range many {
		many[i] = key(t, fmt.Sprint("k", i))
	}
	if _, err = Parse([]byte(`{"keys":[` + strings.Join(many, ",") + `]}`)); err == nil {
		t.Fatal("more keys than the cap accepted")
	}
	if keys, err = Parse([]byte(`{"keys":[` + strings.Join(many[1:], ",") + `]}`)); err != nil || len(keys) != MaxKeys {
		t.Fatalf("a document at the cap = %d keys, %v", len(keys), err)
	}
}

func TestParseRefusesWhatTheStrictTunnelParserRefused(t *testing.T) {
	good := key(t, "good")
	for name, doc := range map[string]string{
		"empty":            ``,
		"not json":         `not json`,
		"trailing value":   `{"keys":[` + good + `]}{}`,
		"unknown field":    `{"keys":[` + good + `],"extra":1}`,
		"too deep":         `{"keys":[` + good + `],"withdrawn_key_ids":` + strings.Repeat("[", 70) + strings.Repeat("]", 70) + `}`,
		"no keys":          `{"keys":[]}`,
		"missing use":      `{"keys":[` + strings.Replace(good, `"use":"sig",`, "", 1) + `]}`,
		"overlong kid":     `{"keys":[` + key(t, strings.Repeat("k", 129)) + `]}`,
		"overlong too big": `{"keys":[` + good + `],"withdrawn_key_ids":["` + strings.Repeat("a", MaxBytes) + `"]}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
