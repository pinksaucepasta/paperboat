package managedssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testCertificate signs key's public part with ca for principals, valid from issued (backdated a minute, like the
// server) for validity.
func testCertificate(t *testing.T, ca ssh.Signer, key ssh.PublicKey, principals []string, issued time.Time, validity time.Duration) string {
	t.Helper()
	certificate := &ssh.Certificate{Key: key, CertType: ssh.UserCert, KeyId: "test", ValidPrincipals: principals,
		ValidAfter: uint64(issued.Add(-time.Minute).Unix()), ValidBefore: uint64(issued.Add(validity).Unix()),
		Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}}}
	if err := certificate.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(certificate)))
}

func testSignerKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// A certificate is reused for its whole life (no control-plane request per connection) and renewed only in its last
// hour; with the control plane unreachable it keeps working to its expiry, also for outages far beyond 10 minutes.
func TestEnsureCertificateReusesUntilItsLastHourAndRidesOutAnOutage(t *testing.T) {
	home := t.TempDir()
	ca, client := testSignerKey(t), testSignerKey(t)
	publicKey := ssh.MarshalAuthorizedKey(client.PublicKey())
	target := CertificateTarget{Home: home, Alias: "studio", OSUser: "alice"}
	start := time.Unix(1_800_000_000, 0)
	const validity, renewal = 12 * time.Hour, time.Hour
	calls := 0
	online := func(at time.Time) func(context.Context) (string, error) {
		return func(context.Context) (string, error) {
			calls++
			return testCertificate(t, ca, client.PublicKey(), []string{"alice", "pb:m"}, at, validity), nil
		}
	}
	offline := func(context.Context) (string, error) { calls++; return "", errors.New("control plane unreachable") }
	ensure := func(issue func(context.Context) (string, error), at time.Time) ([]byte, error) {
		return EnsureCertificate(t.Context(), target, publicKey, issue, at, renewal)
	}
	first, err := ensure(online(start), start)
	if err != nil || calls != 1 {
		t.Fatalf("first: calls=%d err=%v", calls, err)
	}
	for _, later := range []time.Duration{10 * time.Second, 5 * time.Hour, 10*time.Hour + 59*time.Minute} {
		if again, err := ensure(online(start.Add(later)), start.Add(later)); err != nil || calls != 1 || string(again) != string(first) {
			t.Fatalf("+%s: a valid certificate was not reused (calls=%d err=%v)", later, calls, err)
		}
	}
	renewedAt := start.Add(11*time.Hour + 30*time.Minute)
	renewed, err := ensure(online(renewedAt), renewedAt)
	if err != nil || calls != 2 || string(renewed) == string(first) {
		t.Fatalf("a certificate in its last hour was not renewed: calls=%d err=%v", calls, err)
	}
	// Outages longer than 10 minutes (and one into the certificate's last hour, when renewal fails): still usable.
	for _, outage := range []time.Duration{11 * time.Minute, 11*time.Hour + 30*time.Minute} {
		value, err := ensure(offline, renewedAt.Add(outage))
		if err != nil || string(value) != string(renewed) {
			t.Fatalf("outage of %s: err=%v", outage, err)
		}
	}
	if _, err := ensure(offline, renewedAt.Add(validity+time.Second)); err == nil {
		t.Fatal("an expired certificate was used while the control plane is unreachable")
	}
	wrongUser := CertificateTarget{Home: home, Alias: "other", OSUser: "bob"}
	if _, err := EnsureCertificate(t.Context(), wrongUser, publicKey, online(start), start, renewal); err == nil {
		t.Fatal("a certificate for another OS user was accepted from the server")
	}
}

func TestRecentlyUsedCertificatesListsOnlyRecentUse(t *testing.T) {
	home := t.TempDir()
	for _, alias := range []string{"recent", "old"} {
		if err := MarkCertificateUsed(home, alias); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(strings.TrimSuffix(ManagedCertificatePath(home, "old"), "-cert.pub")+"-used", old, old); err != nil {
		t.Fatal(err)
	}
	if got := RecentlyUsedCertificates(home, time.Now().Add(-24*time.Hour)); len(got) != 1 || got[0] != "recent" {
		t.Fatalf("recently used=%v", got)
	}
}
