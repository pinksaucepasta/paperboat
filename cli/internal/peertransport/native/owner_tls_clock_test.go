package native

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// F21-02: peers stamp and judge QUIC leaves on the server clock. A leaf stamped on the server clock (NotBefore
// backdated a minute) is accepted whichever way this machine's clock is off; one outside it is refused by name.
func TestBoundTLSJudgesPeerLeavesOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peer := tailnet.NetworkBinding{QUICPublicKey: base64.RawURLEncoding.EncodeToString(public)}
	verify := boundTLS(&tls.Config{}, peer, false).VerifyConnection
	for name, localAhead := range map[string]time.Duration{"local 10 min ahead": 10 * time.Minute, "local 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverclock.Observe(time.Now().Add(-localAhead).UTC().Format(http.TimeFormat), time.Now(), time.Now())
			server := serverclock.Now()
			fresh := issueTestLeaf(t, private, server.Add(-time.Minute), server.Add(time.Hour), 1)
			if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{fresh}}); err != nil {
				t.Fatalf("server-clock leaf refused: %v", err)
			}
			early := issueTestLeaf(t, private, server.Add(9*time.Minute), server.Add(time.Hour), 2)
			err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{early}})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "server-clock time") {
				t.Fatalf("not-yet-valid leaf error = %v, want a named validity refusal", err)
			}
		})
	}
}
