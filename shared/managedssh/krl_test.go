package managedssh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func krlTestSigner(t *testing.T) ssh.Signer {
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

func krlTestCertificate(t *testing.T, ca, user ssh.Signer, keyID string) []byte {
	t.Helper()
	now := time.Now()
	certificate := &ssh.Certificate{Key: user.PublicKey(), CertType: ssh.UserCert, KeyId: keyID, ValidPrincipals: []string{"alice"},
		ValidAfter: uint64(now.Add(-time.Minute).Unix()), ValidBefore: uint64(now.Add(time.Hour).Unix())}
	if err := certificate.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return ssh.MarshalAuthorizedKey(certificate)
}

// A machine with more revoked key IDs than the old 4096 cap still gets a KRL that OpenSSH reads and that revokes the last one.
func TestKRLListsMoreThan4096KeyIDs(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is unavailable to cross-check the KRL")
	}
	ca, user := krlTestSigner(t), krlTestSigner(t)
	ids := make([]string, 5000)
	for index := range ids {
		ids[index] = fmt.Sprintf("paperboat:user_a:session_%05d:machine_1", index)
	}
	dir := t.TempDir()
	krlPath, certPath := filepath.Join(dir, "revoked.krl"), filepath.Join(dir, "cert.pub")
	if err := os.WriteFile(krlPath, KRL([][]byte{ca.PublicKey().Marshal()}, ids, 1, 1_800_000_000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, krlTestCertificate(t, ca, user, ids[len(ids)-1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(keygen, "-Q", "-f", krlPath, certPath).CombinedOutput(); err == nil || !strings.Contains(string(output), "REVOKED") {
		t.Fatalf("5000th key ID is not revoked: %v %s", err, output)
	}
}

// The KRL revokes exactly the listed key IDs of our CAs: OpenSSH's own reader (ssh-keygen -Q, the code sshd's
// RevokedKeys uses) reports the revoked user's certificate REVOKED and another user's certificate from the same CA ok.
func TestKRLRevokesOnlyTheListedKeyIDs(t *testing.T) {
	empty := KRL(nil, nil, 1, 1_800_000_000)
	if !bytes.HasPrefix(empty, []byte("SSHKRL\n\x00\x00\x00\x00\x01")) {
		t.Fatalf("header=%q", empty[:12])
	}
	ca, user := krlTestSigner(t), krlTestSigner(t)
	revoked, other := "paperboat:user_a:session_a:machine_1", "paperboat:user_b:session_b:machine_1"
	krl := KRL([][]byte{ca.PublicKey().Marshal()}, []string{revoked, revoked}, 2, 1_800_000_000)
	if bytes.Count(krl, []byte(revoked)) != 1 || bytes.Contains(krl, []byte(other)) {
		t.Fatal("key IDs are not listed exactly once")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is unavailable to cross-check the KRL")
	}
	dir := t.TempDir()
	write := func(name string, value []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	krlPath := write("revoked.krl", krl)
	check := func(name string, value []byte) (string, error) {
		output, err := exec.Command(keygen, "-Q", "-f", krlPath, write(name, value)).CombinedOutput()
		return string(output), err
	}
	if output, err := check("a-cert.pub", krlTestCertificate(t, ca, user, revoked)); err == nil || !strings.Contains(output, "REVOKED") {
		t.Fatalf("revoked certificate passed: %v %s", err, output)
	}
	if output, err := check("b-cert.pub", krlTestCertificate(t, ca, user, other)); err != nil || !strings.Contains(output, "ok") {
		t.Fatalf("another user's certificate is refused: %v %s", err, output)
	}
	if output, err := check("other-ca-cert.pub", krlTestCertificate(t, krlTestSigner(t), user, revoked)); err != nil {
		t.Fatalf("a certificate of another CA with the same key ID is refused: %v %s", err, output)
	}
	empties := write("empty.krl", empty)
	if output, err := exec.Command(keygen, "-Q", "-f", empties, write("c-cert.pub", krlTestCertificate(t, ca, user, revoked))).CombinedOutput(); err != nil {
		t.Fatalf("the empty KRL is unreadable or revokes: %v %s", err, output)
	}
}
