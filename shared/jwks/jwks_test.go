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

// A withdrawn key id stays withdrawn: an older document served after the withdrawal (a rolling rotation
// behind several control-plane instances) must not trust it again; a refused document remembers nothing.
func TestTrustNeverRetrustsAWithdrawnKey(t *testing.T) {
	oldKey, newKey := key(t, "old"), key(t, "new")
	var trust Trust
	if _, err := trust.Apply([]byte(`{"keys":[` + oldKey + `,` + newKey + `],"withdrawn_key_ids":["old"]}`)); err != nil {
		t.Fatal(err)
	}
	keys, err := trust.Apply([]byte(`{"keys":[` + oldKey + `,` + newKey + `]}`))
	if err != nil || len(keys) != 1 || keys["new"] == nil {
		t.Fatalf("stale document re-trusted old: %v, %v", keys, err)
	}
	if _, err = trust.Apply([]byte(`{"keys":[` + oldKey + `]}`)); err == nil {
		t.Fatal("a stale document whose only key is withdrawn was accepted")
	}
	// A document refused as a whole withdraws nothing.
	var fresh Trust
	if _, err = fresh.Apply([]byte(`{"keys":[` + newKey + `],"withdrawn_key_ids":["new"]}`)); err == nil {
		t.Fatal("all-withdrawn accepted")
	}
	if keys, err = fresh.Apply([]byte(`{"keys":[` + newKey + `]}`)); err != nil || keys["new"] == nil {
		t.Fatalf("a refused document withdrew a key: %v, %v", keys, err)
	}
}

func TestTrustBoundsTheWithdrawnSet(t *testing.T) {
	var trust Trust
	for i := 0; i < maxWithdrawn/MaxKeys; i++ {
		ids := make([]string, MaxKeys)
		for j := range ids {
			ids[j] = fmt.Sprintf(`"w%d-%d"`, i, j)
		}
		if _, err := trust.Apply([]byte(`{"keys":[` + key(t, "k") + `],"withdrawn_key_ids":[` + strings.Join(ids, ",") + `]}`)); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
	if _, err := trust.Apply([]byte(`{"keys":[` + key(t, "k") + `],"withdrawn_key_ids":["one-too-many"]}`)); err == nil {
		t.Fatal("the withdrawn set grew past its bound")
	}
}
