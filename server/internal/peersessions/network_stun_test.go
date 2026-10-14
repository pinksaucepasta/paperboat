package peersessions

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

func TestRegionalCandidatesSignedSTUNServers(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := mint.New([]mint.Key{{ID: "stun-test", PrivateKey: private}}, "stun-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	for _, servers := range [][]string{nil, {"stun.example.com:3478"}} {
		opts := NetworkOptions{STUNServers: append([]string(nil), servers...)}
		svc, err := NewNetworkService(&db.DB{}, signer, "https://control.example.com", opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(opts.STUNServers) > 0 {
			opts.STUNServers[0] = "mutated.example.com:3478"
		}
		token, err := svc.signRegionalCandidates(map[string]any{"schema": "paperboat.regional-candidates.v1", "nodes": []any{}, "aud": "paperboat-regional-candidates", "account_id": "account", "endpoint_id": "endpoint", "generation": 1, "iat": 100, "exp": 160})
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(token, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		if !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
			t.Fatal("invalid candidate signature")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims map[string]json.RawMessage
		if err = json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if len(servers) == 0 {
			if _, ok := claims["stun_servers"]; ok {
				t.Fatal("empty STUN claim emitted")
			}
			continue
		}
		var got []string
		if err = json.Unmarshal(claims["stun_servers"], &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != servers[0] {
			t.Fatalf("signed servers = %v", got)
		}
	}
	if _, err := NewNetworkService(&db.DB{}, signer, "https://control.example.com", NetworkOptions{STUNServers: []string{"127.0.0.1:3478"}}); err == nil {
		t.Fatal("constructor accepted invalid STUN endpoint")
	}
}
