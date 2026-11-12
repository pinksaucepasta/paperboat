package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/shared/clockleeway"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// F21-02: the CLI's QUIC leaf is judged by the target on the server clock, so it is stamped on it. Stamped on
// a local clock 10 min ahead it started 9 min in a correctly clocked target's future and the handshake was
// refused; 10 min behind it expired 10 min early.
func TestCLILeafIsStampedOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	_, quicPrivate, _ := ed25519.GenerateKey(rand.Reader)
	for name, localAhead := range map[string]time.Duration{"local 10 min ahead": 10 * time.Minute, "local 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverclock.Observe(time.Now().Add(-localAhead).UTC().Format(http.TimeFormat), time.Now(), time.Now())
			server := serverclock.Now().UTC().Truncate(time.Second)
			certificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: "account_1", Role: endpointidentity.RoleCLI, EndpointID: "cli_1", QUICPublicKey: quicPrivate.Public().(ed25519.PublicKey), Generation: 1, Serial: 1, IssuedAt: server.Add(-time.Minute), ExpiresAt: server.Add(30 * 24 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := issueCLILeaf(clientauthority.Authority{RootPublic: rootPublic, LocalKeys: config.PeerIdentityKeys{QUICPrivate: quicPrivate}, LocalCertificate: certificate})
			if err != nil {
				t.Fatalf("leaf for a server-valid certificate refused: %v", err)
			}
			if now := serverclock.Now(); now.Before(leaf.Leaf.NotBefore) || !now.Before(leaf.Leaf.NotAfter) || now.Sub(leaf.Leaf.NotBefore) < clockleeway.Leeway || now.Sub(leaf.Leaf.NotBefore) > clockleeway.Leeway+time.Minute {
				t.Fatalf("leaf valid %s..%s, want it stamped from server-clock %s minus the clock leeway", leaf.Leaf.NotBefore, leaf.Leaf.NotAfter, now)
			}
		})
	}
}
