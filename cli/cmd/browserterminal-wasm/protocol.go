package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

const (
	websocketSubprotocol = "paperboat.browser-terminal.e2ee.v1"
	innerALPN            = "paperboat.browser-terminal.e2ee.v1"

	identityEnvelopeMax = 8 << 10
	appStructuredMax    = protocol.MaxStructuredFrame
	appBinaryMax        = protocol.MaxBinaryFrame
	websocketMessageMax = 256 << 10
	clientCertLifetime  = 10 * time.Minute

	appKindStructured byte = 1
	appKindBinary     byte = 2
)

var errInvalidPeerIdentity = errors.New("machine identity could not be verified")

type identityEnvelope struct {
	Certificate string `json:"certificate"`
	RootKeyID   string `json:"root_key_id"`
}

type machineIdentity struct {
	Certificate endpointidentity.Certificate
	RootPublic  ed25519.PublicKey
	Raw         []byte
}

type browserIdentity struct {
	PrivateKey  ed25519.PrivateKey
	TLS         tls.Certificate
	Fingerprint string
}

// newBrowserIdentity creates a key used for one browser attachment. The key is
// never persisted or shared with the control plane; only its SPKI fingerprint
// is used to bind the one-use server ticket to this TLS client certificate.
func newBrowserIdentity(now time.Time) (*browserIdentity, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate temporary browser key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("generate temporary browser certificate serial: %w", err)
	}
	start := now.UTC().Add(-time.Minute).Truncate(time.Second)
	end := start.Add(clientCertLifetime).Truncate(time.Second)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Paperboat temporary browser terminal"},
		NotBefore:    start,
		NotAfter:     end,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("create temporary browser certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("parse temporary browser certificate: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("encode temporary browser public key: %w", err)
	}
	fingerprint := sha256.Sum256(spki)
	return &browserIdentity{
		PrivateKey:  private,
		TLS:         tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf},
		Fingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]),
	}, nil
}

func verifyIdentityEnvelope(data []byte, rootPublic ed25519.PublicKey, rootKeyID, ownerAccountID, machineID string, now time.Time) (machineIdentity, error) {
	if len(data) == 0 || len(data) > identityEnvelopeMax || len(rootPublic) != ed25519.PublicKeySize || rootKeyID == "" || ownerAccountID == "" || machineID == "" {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	derivedKeyID := accountRootKeyID(rootPublic)
	if rootKeyID != derivedKeyID {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope identityEnvelope
	if err := decoder.Decode(&envelope); err != nil || envelope.Certificate == "" || envelope.RootKeyID != rootKeyID {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Certificate)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != envelope.Certificate {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	certificate, err := endpointidentity.Verify(raw, rootPublic, endpointidentity.Expected{
		AccountID:  ownerAccountID,
		Role:       endpointidentity.RoleMachine,
		EndpointID: machineID,
	}, now)
	if err != nil {
		return machineIdentity{}, fmt.Errorf("%w: %v", errInvalidPeerIdentity, err)
	}
	return machineIdentity{Certificate: certificate, RootPublic: append(ed25519.PublicKey(nil), rootPublic...), Raw: raw}, nil
}

func accountRootKeyID(public ed25519.PublicKey) string {
	digest := sha256.Sum256(public)
	return "aek_" + hex.EncodeToString(digest[:])
}

func verifyMachineTLSLeaf(state tls.ConnectionState, identity machineIdentity, now time.Time) error {
	if len(state.PeerCertificates) != 1 || state.NegotiatedProtocol != innerALPN {
		return errInvalidPeerIdentity
	}
	leaf := state.PeerCertificates[0]
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+time.Minute {
		return errInvalidPeerIdentity
	}
	public, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(public, identity.Certificate.Claims.QUICPublicKey) {
		return errInvalidPeerIdentity
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		return errInvalidPeerIdentity
	}
	return nil
}

func tlsClientConfig(identity *browserIdentity, peer machineIdentity, now func() time.Time) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		NextProtos:             []string{innerALPN},
		Certificates:           []tls.Certificate{identity.TLS},
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true, // The machine uses a self-signed key certified by the independently pinned account root.
		VerifyConnection: func(state tls.ConnectionState) error {
			verified, err := endpointidentity.Verify(peer.Raw, peer.RootPublic, endpointidentity.Expected{
				AccountID:  peer.Certificate.Claims.AccountID,
				Role:       endpointidentity.RoleMachine,
				EndpointID: peer.Certificate.Claims.EndpointID,
				Generation: peer.Certificate.Claims.Generation,
			}, now())
			if err != nil || verified.Claims.Generation != peer.Certificate.Claims.Generation {
				return errInvalidPeerIdentity
			}
			return verifyMachineTLSLeaf(state, peer, now())
		},
	}
}

func writeApplicationFrame(w io.Writer, kind byte, payload []byte) error {
	maximum := appStructuredMax
	if kind == appKindBinary {
		maximum = appBinaryMax
	}
	if kind != appKindStructured && kind != appKindBinary || len(payload) == 0 || len(payload) > maximum {
		return errors.New("invalid terminal application frame")
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	return writeFull(w, payload)
}

func readApplicationFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	kind := header[0]
	maximum := appStructuredMax
	if kind == appKindBinary {
		maximum = appBinaryMax
	} else if kind != appKindStructured {
		return 0, nil, errors.New("unknown terminal application frame kind")
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length == 0 || length > uint32(maximum) {
		return 0, nil, errors.New("terminal application frame length is invalid")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return kind, payload, nil
}

func writeFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
