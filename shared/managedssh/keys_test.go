package managedssh

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"net/netip"
	"slices"
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

// Each user's loopback source is stable, inside the range the certificates and CA lines allow, one of the addresses
// the macOS guard aliases, and different users are spread over the pool.
func TestLoopbackSourceIsStablePerUserAndInsideTheTrustedRange(t *testing.T) {
	trusted := netip.MustParsePrefix("127.0.0.0/8")
	pool := LoopbackSources()
	seen := map[netip.Addr]bool{}
	for index := range 200 {
		user := fmt.Sprintf("user_%d", index)
		source := LoopbackSource(user)
		if source != LoopbackSource(user) || !trusted.Contains(source) || !slices.Contains(pool, source) || source == netip.MustParseAddr("127.0.0.1") {
			t.Fatalf("%s source %s", user, source)
		}
		seen[source] = true
	}
	if len(seen) < 40 {
		t.Fatalf("200 users use only %d of %d sources", len(seen), len(pool))
	}
	if !strings.Contains(CATrustFrom, "127.0.0.0/8") || !strings.HasPrefix(CertificateSourceAddress, "127.0.0.0/8,") {
		t.Fatalf("trusted ranges %s %s", CATrustFrom, CertificateSourceAddress)
	}
}
