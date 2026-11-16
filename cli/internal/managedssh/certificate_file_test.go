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

func TestEnsureCertificateRenewsEachConnectionAndRidesOutAnOutage(t *testing.T) {
	home := t.TempDir()
	ca, client := testSignerKey(t), testSignerKey(t)
	publicKey := ssh.MarshalAuthorizedKey(client.PublicKey())
	target := CertificateTarget{Home: home, Alias: "studio", OSUser: "alice"}
	start := time.Unix(1_800_000_000, 0)
	const validity = 12 * time.Hour
	calls := 0
	online := func(at time.Time) func(context.Context) (string, error) {
		return func(context.Context) (string, error) {
			calls++
			return testCertificate(t, ca, client.PublicKey(), []string{"alice", "pb:m:0"}, at, validity), nil
		}
	}
	offline := func(context.Context) (string, error) { calls++; return "", errors.New("control plane unreachable") }

	first, err := EnsureCertificate(t.Context(), target, publicKey, online(start), start, 30*time.Second)
	if err != nil || calls != 1 {
		t.Fatalf("first: calls=%d err=%v", calls, err)
	}
	if again, err := EnsureCertificate(t.Context(), target, publicKey, online(start), start.Add(10*time.Second), 30*time.Second); err != nil || calls != 1 || string(again) != string(first) {
		t.Fatalf("a certificate younger than the reuse window was not reused: calls=%d err=%v", calls, err)
	}
	renewed, err := EnsureCertificate(t.Context(), target, publicKey, online(start.Add(time.Minute)), start.Add(time.Minute), 30*time.Second)
	if err != nil || calls != 2 || string(renewed) == string(first) {
		t.Fatalf("an older certificate was not renewed: calls=%d err=%v", calls, err)
	}
	// The control plane is unreachable for longer than 10 minutes (and for 11 hours): the certificate keeps working.
	for _, outage := range []time.Duration{11 * time.Minute, 11 * time.Hour} {
		value, err := EnsureCertificate(t.Context(), target, publicKey, offline, start.Add(time.Minute+outage), 30*time.Second)
		if err != nil || string(value) != string(renewed) {
			t.Fatalf("outage of %s: err=%v", outage, err)
		}
	}
	if _, err := EnsureCertificate(t.Context(), target, publicKey, offline, start.Add(time.Minute+validity+time.Second), 30*time.Second); err == nil {
		t.Fatal("an expired certificate was used while the control plane is unreachable")
	}
	wrongUser := CertificateTarget{Home: home, Alias: "studio", OSUser: "bob"}
	if _, err := EnsureCertificate(t.Context(), wrongUser, publicKey, online(start), start, 30*time.Second); err == nil {
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
