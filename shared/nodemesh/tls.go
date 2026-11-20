package nodemesh

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrPeerUnknown: the peer's certificate is not in the directory.
	ErrPeerUnknown = errors.New("node mesh peer is not in the directory")
	// ErrPinMismatch: the dialled node did not present the pinned certificate.
	ErrPinMismatch = errors.New("node mesh peer certificate does not match its pin")
)

const pinPrefix = "sha256:"

// SPKIPin is the pin of a certificate: "sha256:" + hex of the SHA-256 of its SubjectPublicKeyInfo.
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return pinPrefix + hex.EncodeToString(sum[:])
}

// ParsePin checks the canonical pin form and returns the digest.
func ParsePin(pin string) ([]byte, error) {
	raw, ok := strings.CutPrefix(strings.ToLower(pin), pinPrefix)
	if !ok || len(raw) != 2*sha256.Size {
		return nil, fmt.Errorf("%w: pin", ErrInvalid)
	}
	digest, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: pin", ErrInvalid)
	}
	return digest, nil
}

// PinFromDigest formats a raw SHA-256 SPKI digest as a pin.
func PinFromDigest(digest []byte) string { return pinPrefix + hex.EncodeToString(digest) }

func leafWithin(raw [][]byte, now time.Time) (*x509.Certificate, error) {
	if len(raw) == 0 {
		return nil, ErrPeerUnknown
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return nil, ErrPeerUnknown
	}
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("%w: certificate outside its validity window", ErrPeerUnknown)
	}
	return leaf, nil
}

// ServerTLS accepts only nodes whose certificate pin is in peers() at handshake time. cert is the node's own
// registered certificate (the one whose pin the server published). TLS 1.3 only.
func ServerTLS(cert tls.Certificate, alpn string, peers func() PeerSet) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{alpn},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := leafWithin(raw, time.Now())
			if err != nil {
				return err
			}
			if _, ok := peers().ByPin(SPKIPin(leaf)); !ok {
				return ErrPeerUnknown
			}
			return nil
		},
	}
}

// ClientTLS dials peer p: it presents cert and accepts only p's pinned certificate. There is no server name
// (the pin is the identity), so the ClientHello carries no SNI.
func ClientTLS(cert tls.Certificate, alpn string, p Peer) (*tls.Config, error) {
	want, err := ParsePin(p.SPKISHA256)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
		InsecureSkipVerify: true, // the pin below is the verification
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := leafWithin(raw, time.Now())
			if err != nil {
				return err
			}
			sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return ErrPinMismatch
			}
			return nil
		},
	}, nil
}

// PeerOf identifies the peer on the far side of a completed handshake by its certificate pin.
func PeerOf(state tls.ConnectionState, set PeerSet) (Peer, bool) {
	if len(state.PeerCertificates) == 0 {
		return Peer{}, false
	}
	return set.ByPin(SPKIPin(state.PeerCertificates[0]))
}
