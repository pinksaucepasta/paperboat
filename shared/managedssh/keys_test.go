package managedssh

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"golang.org/x/crypto/ssh"
	"strings"
	"testing"
)

func TestHostKeyInventoryCanonicalWireFingerprint(t *testing.T) {
	keys := []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINdamAGCsQq31Uv+08lkBzoO4XLz2qYjJa8CGmj3B1Ea", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID1AF8PoQ4lakrcKp00bfrycmCzPLsSWjMDNVfEq9GYM"}
	for _, values := range [][]string{keys, {keys[1] + " fixture comment", keys[0]}} {
		inventory, err := ParseHostPublicKeys(values)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(inventory.Fingerprint[:]); got != "bf2be74167e23ed0b5feff854532af26ddb1a7030e949c1aab96a7feb1ee424c" {
			t.Fatalf("wire fingerprint=%s", got)
		}
		if len(inventory.Keys) != 2 {
			t.Fatal("host key omitted")
		}
	}
	limit := make([]string, 9)
	for i := range limit {
		key, err := ssh.NewPublicKey(ed25519.PublicKey(bytes.Repeat([]byte{byte(i + 1)}, 32)))
		if err != nil {
			t.Fatal(err)
		}
		limit[i] = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	}
	if _, err := ParseHostPublicKeys(limit[:8]); err != nil {
		t.Fatalf("exact limit rejected: %v", err)
	}
	for name, values := range map[string][]string{"empty": nil, "duplicate": {keys[0], keys[0]}, "options": {"command=\"id\" " + keys[0]}, "oversize": {strings.Repeat("x", 8193)}, "nul": {keys[0] + "\x00"}, "limit": limit} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHostPublicKeys(values); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid inventory accepted: %v", err)
			}
		})
	}
}
