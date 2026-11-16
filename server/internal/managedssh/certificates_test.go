package managedssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"golang.org/x/crypto/ssh"
)

var testCertificates = CertificateConfig{EncryptionKey: "test-encryption-key-0123456789abcdef", Validity: 12 * time.Hour}

func issueTestCertificate(t *testing.T, service *Service, now time.Time) (*ssh.Certificate, ssh.Signer) {
	t.Helper()
	return issueSessionCertificate(t, service, now, "client_01")
}

func issueSessionCertificate(t *testing.T, service *Service, now time.Time, session string) (*ssh.Certificate, ssh.Signer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.IssueCertificate(context.Background(), IssueCertificateRequest{UserID: "user_01", CLIClientSessionID: session, UserMachineID: "machine_01", MachineGeneration: 2, PublicKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(result.Certificate))
	if err != nil {
		t.Fatal(err)
	}
	certificate, ok := parsed.(*ssh.Certificate)
	if !ok || result.OSUser != "alice" || !result.ValidBefore.Equal(now.Add(testCertificates.Validity)) {
		t.Fatalf("result=%+v", result)
	}
	return certificate, signer
}

// checkCertificate verifies like sshd does for a `cert-authority,principals="<principal>"` line: signed by a trusted
// CA, valid now, carrying the line's principal, critical options known (sshd also enforces source-address;
// CertChecker does not).
func checkCertificate(certificate *ssh.Certificate, principal string, trusted []string, now time.Time, source string) error {
	checker := ssh.CertChecker{
		Clock: func() time.Time { return now },
		IsUserAuthority: func(authority ssh.PublicKey) bool {
			for _, key := range trusted {
				parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
				if err == nil && bytes.Equal(parsed.Marshal(), authority.Marshal()) {
					return true
				}
			}
			return false
		},
		SupportedCriticalOptions: []string{"source-address"},
	}
	if _, err := checker.Authenticate(fakeConn{user: principal, remote: source}, certificate); err != nil {
		return err
	}
	return nil
}

type fakeConn struct {
	ssh.ConnMetadata
	user, remote string
}

func (c fakeConn) User() string          { return c.user }
func (c fakeConn) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.ParseIP(c.remote), Port: 50000} }
func (c fakeConn) SessionID() []byte     { return []byte("session") }
func (c fakeConn) ClientVersion() []byte { return []byte("SSH-2.0-test") }

func TestIssuedCertificateIsAcceptedOnlyForItsUserCAWindowAndLoopback(t *testing.T) {
	repository := &fakeRepository{osUser: "alice"}
	service, err := NewService(repository, testCertificates)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	certificate, _ := issueTestCertificate(t, service, now)
	set, err := service.TrustedUserCAs(context.Background(), UserCAsRequest{ActorUserID: "owner_01", UserMachineID: "machine_01", MachineGeneration: 2, Now: now})
	trusted, principal := set.Keys, set.Principal
	if err != nil || len(trusted) != 1 || principal != "pb:machine_01" {
		t.Fatalf("trusted=%+v err=%v", set, err)
	}
	if certificate.CertType != ssh.UserCert || strings.Join(certificate.ValidPrincipals, ",") != "alice,pb:machine_01" || !strings.HasPrefix(certificate.KeyId, "paperboat:user_01:client_01:machine_01") {
		t.Fatalf("certificate=%+v", certificate)
	}
	if err := checkCertificate(certificate, principal, trusted, now, "127.0.0.1"); err != nil {
		t.Fatalf("valid certificate refused: %v", err)
	}
	// sshd refuses a login from any other address (x/crypto's CertChecker leaves source-address to its server).
	if certificate.CriticalOptions["source-address"] != "127.0.0.0/8,::1/128" {
		t.Fatalf("critical options=%v", certificate.CriticalOptions)
	}
	// An outage longer than 10 minutes (here 11 minutes and 11 hours) keeps the certificate usable: no renewal needed.
	for _, outage := range []time.Duration{11 * time.Minute, 11 * time.Hour} {
		if err := checkCertificate(certificate, principal, trusted, now.Add(outage), "127.0.0.1"); err != nil {
			t.Fatalf("certificate refused %s into a control-plane outage: %v", outage, err)
		}
	}
	refusals := map[string]error{
		"expired":         checkCertificate(certificate, principal, trusted, now.Add(testCertificates.Validity+time.Second), "127.0.0.1"),
		"not yet valid":   checkCertificate(certificate, principal, trusted, now.Add(-2*time.Minute), "127.0.0.1"),
		"wrong principal": checkCertificate(certificate, "pb:machine_02:0", trusted, now, "127.0.0.1"),
		"wrong CA":        checkCertificate(certificate, principal, []string{publicLine(t, "ed25519")}, now, "127.0.0.1"),
	}
	for name, err := range refusals {
		if err == nil {
			t.Errorf("%s certificate was accepted", name)
		}
	}
}

// Revoking user A lists only A's key ID for the machine; the principal stays, so B's existing certificate keeps
// working with no renewal, and a host refusing the listed key IDs refuses only A.
func TestRevocationListsOnlyTheRevokedHolderAndLeavesOthersAlone(t *testing.T) {
	repository := &fakeRepository{osUser: "alice"}
	service, _ := NewService(repository, testCertificates)
	now := time.Unix(1_800_000_000, 0).UTC()
	request := UserCAsRequest{ActorUserID: "owner_01", UserMachineID: "machine_01", MachineGeneration: 2, Now: now}
	userA, _ := issueSessionCertificate(t, service, now, "client_a")
	userB, _ := issueSessionCertificate(t, service, now, "client_b")
	before, _ := service.TrustedUserCAs(context.Background(), request)
	repository.revoked = []string{CertificateKeyID("user_01", "client_a", "machine_01")}
	after, _ := service.TrustedUserCAs(context.Background(), request)
	if after.Principal != before.Principal || after.Principal != MachinePrincipal("machine_01") {
		t.Fatalf("the principal changed on a revocation: %q -> %q", before.Principal, after.Principal)
	}
	refused := func(certificate *ssh.Certificate) bool {
		return slices.Contains(after.RevokedKeyIDs, certificate.KeyId)
	}
	if !refused(userA) || refused(userB) || len(after.RevokedKeyIDs) != 1 {
		t.Fatalf("revoked=%v A=%q B=%q", after.RevokedKeyIDs, userA.KeyId, userB.KeyId)
	}
	if err := checkCertificate(userB, after.Principal, after.Keys, now.Add(time.Hour), "127.0.0.1"); err != nil {
		t.Fatalf("B's existing certificate stopped working after A's revocation: %v", err)
	}
}

func TestRotationKeepsIssuedCertificatesTrusted(t *testing.T) {
	repository := &fakeRepository{osUser: "alice"}
	service, _ := NewService(repository, testCertificates)
	now := time.Unix(1_800_000_000, 0).UTC()
	before, _ := issueTestCertificate(t, service, now)
	cas, err := service.RotateUserCA(context.Background(), "owner_01", now)
	if err != nil || len(cas) != 2 {
		t.Fatalf("cas=%v err=%v", cas, err)
	}
	set, _ := service.TrustedUserCAs(context.Background(), UserCAsRequest{ActorUserID: "owner_01", UserMachineID: "machine_01", MachineGeneration: 2, Now: now})
	trusted := set.Keys
	if len(trusted) != 2 || checkCertificate(before, set.Principal, trusted, now, "127.0.0.1") != nil {
		t.Fatalf("certificate from the old CA refused while the new one is pending: trusted=%v", trusted)
	}
	// Promotion: the pending CA signs, the old one is retiring and still trusted.
	repository.cas[0].State, repository.cas[1].State = "retiring", "active"
	after, _ := issueTestCertificate(t, service, now)
	if bytes.Equal(after.SignatureKey.Marshal(), before.SignatureKey.Marshal()) {
		t.Fatal("certificate after promotion is signed by the old CA")
	}
	for name, certificate := range map[string]*ssh.Certificate{"old": before, "new": after} {
		if err := checkCertificate(certificate, set.Principal, trusted, now, "127.0.0.1"); err != nil {
			t.Fatalf("%s certificate refused during rotation: %v", name, err)
		}
	}
}

func TestIssueCertificateRefusesBadInputAndUnreadableCA(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	if _, err := NewService(&fakeRepository{}, CertificateConfig{Validity: time.Minute}); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("service without an encryption key: %v", err)
	}
	repository := &fakeRepository{osUser: "alice"}
	service, _ := NewService(repository, testCertificates)
	request := IssueCertificateRequest{UserID: "user_01", CLIClientSessionID: "client_01", UserMachineID: "machine_01", MachineGeneration: 2, PublicKey: publicLine(t, "rsa"), Now: now}
	if _, err := service.IssueCertificate(context.Background(), request); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("RSA client key: %v", err)
	}
	request.PublicKey = `command="id" ` + publicLine(t, "ed25519")
	if _, err := service.IssueCertificate(context.Background(), request); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("key with options: %v", err)
	}
	request.PublicKey = publicLine(t, "ed25519")
	repository.err = ErrUnavailable
	if _, err := service.IssueCertificate(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unauthorized machine or revoked session: %v", err)
	}
	repository.err = nil
	other, _ := NewService(repository, CertificateConfig{EncryptionKey: "another-key-0123456789abcdef0123", Validity: time.Minute})
	if _, err := service.IssueCertificate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := other.IssueCertificate(context.Background(), request); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("CA sealed under another key: %v", err)
	}
	if bytes.Contains(repository.cas[0].PrivateKeyCiphertext, []byte("PRIVATE")) || len(repository.cas[0].PrivateKeyCiphertext) <= ed25519.SeedSize {
		t.Fatal("CA private key is not sealed")
	}
}
