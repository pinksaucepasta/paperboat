package endpointidentity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/clockleeway"
)

func TestCertificateIsDeterministicAndIdentityBound(t *testing.T) {
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	quicPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	claims := Claims{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 3, Serial: 9, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	first, err := Sign(rootPrivate, claims)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Sign(rootPrivate, claims)
	if err != nil {
		t.Fatal(err)
	}
	firstRaw, _ := first.MarshalBinary()
	secondRaw, _ := second.MarshalBinary()
	if !bytes.Equal(firstRaw, secondRaw) || first.Fingerprint() != second.Fingerprint() {
		t.Fatal("same claims produced different certificates")
	}
	verified, err := Verify(firstRaw, rootPublic, Expected{AccountID: claims.AccountID, Role: claims.Role, EndpointID: claims.EndpointID, Generation: claims.Generation}, now)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Claims.Serial != claims.Serial || !bytes.Equal(verified.Claims.QUICPublicKey, claims.QUICPublicKey) {
		t.Fatalf("verified claims=%+v", verified.Claims)
	}
	rootFingerprint, err := RootFingerprint(rootPublic)
	if err != nil || len(rootFingerprint) != 64 {
		t.Fatalf("root fingerprint=%q err=%v", rootFingerprint, err)
	}
}

func TestCertificateRejectsTamperingSubstitutionAndExpiry(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	otherRoot, _, _ := ed25519.GenerateKey(rand.Reader)
	quicPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0).UTC()
	certificate, err := Sign(rootPrivate, Claims{AccountID: "account_01", Role: RoleCLI, EndpointID: "cli_01", QUICPublicKey: quicPublic, Generation: 2, Serial: 4, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := certificate.MarshalBinary()
	checks := []struct {
		name     string
		raw      []byte
		root     ed25519.PublicKey
		expected Expected
		now      time.Time
	}{
		{name: "wrong root", raw: raw, root: otherRoot, expected: Expected{AccountID: "account_01"}, now: now},
		{name: "wrong account", raw: raw, root: rootPublic, expected: Expected{AccountID: "account_02"}, now: now},
		{name: "wrong endpoint", raw: raw, root: rootPublic, expected: Expected{EndpointID: "cli_02"}, now: now},
		{name: "wrong role", raw: raw, root: rootPublic, expected: Expected{Role: RoleMachine}, now: now},
		{name: "wrong generation", raw: raw, root: rootPublic, expected: Expected{Generation: 3}, now: now},
		{name: "expired", raw: raw, root: rootPublic, expected: Expected{AccountID: "account_01"}, now: now.Add(time.Minute + clockleeway.Leeway)},
	}
	tampered := append([]byte(nil), raw...)
	tampered[12] ^= 1
	checks = append(checks, struct {
		name     string
		raw      []byte
		root     ed25519.PublicKey
		expected Expected
		now      time.Time
	}{name: "tampered", raw: tampered, root: rootPublic, expected: Expected{}, now: now})
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if _, err := Verify(check.raw, check.root, check.expected, check.now); err == nil {
				t.Fatal("invalid certificate accepted")
			}
		})
	}
}

func TestTLSLeafRequiresCertifiedQUICKey(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	quicPublic, quicPrivate, _ := ed25519.GenerateKey(rand.Reader)
	_, wrongPrivate, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0).UTC()
	certificate, err := Sign(rootPrivate, Claims{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTLSCertificate(certificate, rootPublic, wrongPrivate, now, time.Hour); err == nil {
		t.Fatal("wrong QUIC private key accepted")
	}
	leaf, err := NewTLSCertificate(certificate, rootPublic, quicPrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := certificate.MarshalBinary()
	config, err := ServerTLS(leaf, PeerExpectation{RootPublic: rootPublic, Certificate: raw, Expected: Expected{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", Generation: 1}}, "paperboat-test-v1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil || config.ClientAuth != tls.RequireAnyClientCert {
		t.Fatalf("config=%+v", config)
	}
}

func TestTLSVerifierRejectsEndpointSubstitutionAndLeafMutation(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	quicPublic, quicPrivate, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0).UTC()
	claims := Claims{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	expected, _ := Sign(rootPrivate, claims)
	expectedRaw, _ := expected.MarshalBinary()
	expectedLeaf, err := NewTLSCertificate(expected, rootPublic, quicPrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ClientTLS(expectedLeaf, PeerExpectation{RootPublic: rootPublic, Certificate: expectedRaw, Expected: Expected{AccountID: claims.AccountID, Role: claims.Role, EndpointID: claims.EndpointID, Generation: 1}}, "paperboat-test-v1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{expectedLeaf.Leaf}, NegotiatedProtocol: "paperboat-test-v1"}
	if err := config.VerifyConnection(state); err != nil {
		t.Fatalf("valid leaf rejected: %v", err)
	}

	substitutedClaims := claims
	substitutedClaims.EndpointID = "machine_02"
	substitutedClaims.Serial = 2
	substituted, _ := Sign(rootPrivate, substitutedClaims)
	substitutedRaw, _ := substituted.MarshalBinary()
	if _, err := ClientTLS(expectedLeaf, PeerExpectation{RootPublic: rootPublic, Certificate: substitutedRaw, Expected: Expected{AccountID: claims.AccountID, Role: claims.Role, EndpointID: claims.EndpointID, Generation: 1}}, "paperboat-test-v1", func() time.Time { return now }); err == nil {
		t.Fatal("substituted endpoint identity accepted")
	}

	mutated := *expectedLeaf.Leaf
	mutated.Signature = append([]byte(nil), mutated.Signature...)
	mutated.Signature[0] ^= 1
	state.PeerCertificates = []*x509.Certificate{&mutated}
	if err := config.VerifyConnection(state); err == nil {
		t.Fatal("mutated leaf signature accepted")
	}
}

func TestTLSVerifierSelectsPeerCertificateFromTrustedKeyID(t *testing.T) {
	localRoot, localRootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	peerRoot, peerRootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	peerQUIC, peerQUICPrivate, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0).UTC()
	local, err := Sign(localRootPrivate, Claims{AccountID: "account_01", Role: RoleCLI, EndpointID: "cli_01", QUICPublicKey: localRoot, Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// The local endpoint's QUIC key must be distinct from its signing key.
	localQUIC, localQUICPrivate, _ := ed25519.GenerateKey(rand.Reader)
	local.Claims.QUICPublicKey = localQUIC
	local, err = Sign(localRootPrivate, local.Claims)
	if err != nil {
		t.Fatal(err)
	}
	localLeaf, err := NewTLSCertificate(local, localRoot, localQUICPrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := Sign(peerRootPrivate, Claims{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", QUICPublicKey: peerQUIC, Generation: 2, Serial: 2, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	peerRaw, err := peer.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	keyID := func(public ed25519.PublicKey) string {
		fingerprint := sha256.Sum256(public)
		return "aek_" + hex.EncodeToString(fingerprint[:])
	}
	key := func(public ed25519.PublicKey) TrustedKey {
		return TrustedKey{KeyID: keyID(public), PublicKey: public, Fingerprint: sha256.Sum256(public), Generation: 1}
	}
	trusted := []TrustedKey{key(localRoot), key(peerRoot)}
	config, err := ClientTLS(localLeaf, PeerExpectation{RootPublic: localRoot, TrustedKeys: trusted, CertificateKeyID: keyID(peerRoot), Certificate: peerRaw, Expected: Expected{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", Generation: 2}}, "paperboat-test-v1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	peerLeaf, err := NewTLSCertificate(peer, peerRoot, peerQUICPrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{peerLeaf.Leaf}, NegotiatedProtocol: "paperboat-test-v1"}
	if err := config.VerifyConnection(state); err != nil {
		t.Fatalf("valid mixed-key peer rejected: %v", err)
	}
	if _, err := ClientTLS(localLeaf, PeerExpectation{RootPublic: localRoot, TrustedKeys: trusted, CertificateKeyID: keyID(localRoot), Certificate: peerRaw, Expected: Expected{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", Generation: 2}}, "paperboat-test-v1", func() time.Time { return now }); err == nil {
		t.Fatal("peer certificate accepted with the wrong trusted key ID")
	}
}

// A machine whose clock is 10 minutes fast or slow (X-27) must still build its QUIC leaf and be accepted by a
// peer on the true time, and the other way round.
func TestTLSLeafAcceptedAcrossTenMinutesOfClockSkew(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	quicPublic, quicPrivate, _ := ed25519.GenerateKey(rand.Reader)
	truth := time.Unix(1_800_000_000, 0).UTC()
	certificate, err := Sign(rootPrivate, Claims{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 1, Serial: 1, IssuedAt: truth.Add(-time.Minute), ExpiresAt: truth.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := certificate.MarshalBinary()
	peer := PeerExpectation{RootPublic: rootPublic, Certificate: raw, Expected: Expected{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", Generation: 1}}
	for _, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		leaf, err := NewTLSCertificate(certificate, rootPublic, quicPrivate, truth.Add(skew), time.Hour)
		if err != nil {
			t.Fatalf("skew %v: leaf: %v", skew, err)
		}
		for _, viewer := range []time.Duration{0, skew, -skew} {
			at := truth.Add(viewer)
			config, err := ClientTLS(leaf, peer, "paperboat-test-v1", func() time.Time { return at })
			if err != nil {
				t.Fatalf("skew %v viewer %v: %v", skew, viewer, err)
			}
			state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf.Leaf}, NegotiatedProtocol: "paperboat-test-v1"}
			if err := config.VerifyConnection(state); err != nil {
				t.Fatalf("skew %v viewer %v: %v", skew, viewer, err)
			}
		}
	}
	if _, err := NewTLSCertificate(certificate, rootPublic, quicPrivate, truth.Add(-2*clockleeway.Leeway), time.Hour); err == nil {
		t.Fatal("a clock 30 minutes off was accepted")
	}
}

// Relays (relay/derpquic Verifier) and peers (native boundTLS) refuse a leaf spanning more than 24 h + 1 min.
func TestTLSLeafSpanStaysWithinTheRelayBound(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(rand.Reader)
	quicPublic, quicPrivate, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0).UTC()
	certificate, err := Sign(rootPrivate, Claims{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(90 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := NewTLSCertificate(certificate, rootPublic, quicPrivate, now, maximumLeafLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if span := leaf.Leaf.NotAfter.Sub(leaf.Leaf.NotBefore); span > 24*time.Hour+time.Minute {
		t.Fatalf("leaf spans %v; relays and peers refuse more than 24h1m", span)
	}
}
