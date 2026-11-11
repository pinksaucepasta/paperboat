package tailnet

import (
	"encoding/base64"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

func encodedNodeKey(t testing.TB) (string, key.NodePublic) {
	t.Helper()
	public := key.NewNode().Public()
	raw := public.Raw32()
	return base64.RawURLEncoding.EncodeToString(raw[:]), public
}

// The recovery loop validates every peer's key on every pass: a valid key is remembered, an invalid one is always
// refused (never remembered), and the memory is bounded.
func TestPublicKeyIsValidatedOnceAndRefusalsAreNeverRemembered(t *testing.T) {
	encoded, want := encodedNodeKey(t)
	for range 3 {
		got, err := publicKey(encoded)
		if err != nil || got != want {
			t.Fatalf("publicKey = %v, %v; want %v", got, err, want)
		}
	}
	publicKeys.Lock()
	_, remembered := publicKeys.valid[encoded]
	publicKeys.Unlock()
	if !remembered {
		t.Fatal("a validated key was not remembered")
	}
	for name, bad := range map[string]string{
		"all zero (low order)": base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"too short":            encoded[:10],
		"padded":               encoded + "=",
		"not base64":           strings.Repeat("!", 43),
	} {
		for range 2 {
			if _, err := publicKey(bad); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		publicKeys.Lock()
		_, cached := publicKeys.valid[bad]
		publicKeys.Unlock()
		if cached {
			t.Errorf("%s was remembered as valid", name)
		}
	}
	publicKeys.Lock()
	for i := len(publicKeys.valid); i < publicKeyCacheLimit; i++ {
		publicKeys.valid["filler-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune(i))] = want
	}
	publicKeys.Unlock()
	for range 5 {
		other, _ := encodedNodeKey(t)
		if _, err := publicKey(other); err != nil {
			t.Fatal(err)
		}
	}
	publicKeys.Lock()
	size := len(publicKeys.valid)
	publicKeys.Unlock()
	if size > publicKeyCacheLimit {
		t.Fatalf("%d keys remembered, limit %d", size, publicKeyCacheLimit)
	}
}

// BenchmarkPublicKey: a remembered key costs a map lookup; an uncached validation was two X25519 scalar multiplications.
func BenchmarkPublicKey(b *testing.B) {
	encoded, _ := encodedNodeKey(b)
	b.Run("remembered", func(b *testing.B) {
		for range b.N {
			if _, err := publicKey(encoded); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("validated", func(b *testing.B) {
		for range b.N {
			if _, err := validatePublicKey(encoded); err != nil {
				b.Fatal(err)
			}
		}
	})
}
