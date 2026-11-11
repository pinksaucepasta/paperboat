package connectorprotocol

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testCertificatePEM(t *testing.T, isCA bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: isCA, BasicConstraintsValid: true}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}))
}

// A route trusts only real CA certificates in PEM, bounded; a leaf, a key, a path or trailing text is refused.
func TestValidateOriginCA(t *testing.T) {
	root := testCertificatePEM(t, true)
	for name, test := range map[string]struct {
		value string
		ok    bool
	}{
		"one CA":              {root, true},
		"root + intermediate": {root + testCertificatePEM(t, true), true},
		"leaf, not a CA":      {testCertificatePEM(t, false), false},
		"a path":              {"/data/caddy/pki/authorities/local/root.crt", false},
		"trailing text":       {root + "junk", false},
		"leading text":        {"junk\n" + root, false},
		"text between":        {root + "junk\n" + root, false},
		"blank lines between": {root + "\n\n" + root, true},
		"a private key":       {root + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", false},
		"nine certificates":   {strings.Repeat(root, 9), false},
		"empty":               {"", false},
	} {
		if err := ValidateOriginCA(test.value); (err == nil) != test.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, test.ok)
		}
	}
	if OriginCAFingerprint(root) == "" || OriginCAFingerprint(root) == OriginCAFingerprint(root+" ") || OriginCAFingerprint("") != "" {
		t.Fatal("fingerprint does not name the exact CA")
	}
}
