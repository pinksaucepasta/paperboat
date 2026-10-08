package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserTerminalWebSocketWriteDeadlineIsBoundedAndRespectsEarlierLimit(t *testing.T) {
	stream := newBrowserTerminalWebSocketStream(context.Background(), nil)
	defer stream.Close()

	defaultContext, cancelDefault := stream.operationContext(false)
	defaultDeadline, ok := defaultContext.Deadline()
	cancelDefault()
	if !ok {
		t.Fatal("WebSocket write has no deadline")
	}
	remaining := time.Until(defaultDeadline)
	if remaining <= 0 || remaining > browserTerminalWriteTimeout {
		t.Fatalf("default write deadline has invalid remaining duration %s", remaining)
	}

	earlier := time.Now().Add(100 * time.Millisecond)
	if err := stream.SetWriteDeadline(earlier); err != nil {
		t.Fatal(err)
	}
	boundedContext, cancelBounded := stream.operationContext(false)
	boundedDeadline, ok := boundedContext.Deadline()
	cancelBounded()
	if !ok || boundedDeadline.After(earlier) {
		t.Fatalf("earlier caller deadline was not preserved: deadline=%v earlier=%v", boundedDeadline, earlier)
	}
}

func TestBrowserTerminalClientCertificateMustMatchSignedKeyPin(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	certificate, digest := browserTerminalTestCertificate(t, now)
	if err := verifyBrowserTerminalClientCertificate([][]byte{certificate}, digest, now); err != nil {
		t.Fatalf("matching ephemeral client certificate rejected: %v", err)
	}
	otherCertificate, otherDigest := browserTerminalTestCertificate(t, now)
	if err := verifyBrowserTerminalClientCertificate([][]byte{certificate}, otherDigest, now); err == nil {
		t.Fatal("client certificate with a different signed SPKI pin was accepted")
	}
	if err := verifyBrowserTerminalClientCertificate([][]byte{otherCertificate}, digest, now); err == nil {
		t.Fatal("different browser private key was accepted for the ticket")
	}
}

func browserTerminalTestCertificate(t *testing.T, now time.Time) ([]byte, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "browser terminal test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(4 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return encoded, base64.RawURLEncoding.EncodeToString(digest[:])
}

// The dedicated comparison endpoint must negotiate the protocol offered by
// its edge caller while ordinary browser terminals retain their own protocol.
type comparisonHandshakeAuthorizer struct{ digest string }

func (a comparisonHandshakeAuthorizer) Authorize(context.Context, protocol.Frame) (Authorization, error) {
	return Authorization{}, nil
}
func (a comparisonHandshakeAuthorizer) BrowserTerminalPublicKeySHA256(context.Context) (string, error) {
	return a.digest, nil
}
func (a comparisonHandshakeAuthorizer) BrowserConfigComparePublicKeySHA256(context.Context) (string, error) {
	return a.digest, nil
}
func TestBrowserComparisonEndpointNegotiatesItsOwnWebSocketProtocol(t *testing.T) {
	now := time.Now().UTC()
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: "account_fixture", Role: endpointidentity.RoleMachine, EndpointID: "machine_fixture", QUICPublicKey: machinePublic, Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := certificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := endpointidentity.NewTLSCertificate(certificate, rootPublic, machinePrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := sha256.Sum256(rootPublic)
	_, pin := browserTerminalTestCertificate(t, now)
	runtime := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) { return Authorization{}, nil }, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		return operation.Outcome{}
	}, 1)
	for _, compare := range []bool{false, true} {
		handler, err := NewBrowserTerminalWebSocketHandler(BrowserTerminalWebSocketHandlerConfig{CompareOnly: compare, Server: runtime, Authorizer: func(string) (Authorizer, error) { return comparisonHandshakeAuthorizer{pin}, nil }, Identity: func(context.Context) (BrowserTerminalIdentity, error) {
			return BrowserTerminalIdentity{Certificate: raw, RootKeyID: "aek_" + hex.EncodeToString(rootDigest[:]), TLSCertificate: leaf}, nil
		}, MaxConnections: 1, MaxMessageBytes: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		origin := httptest.NewServer(handler)
		protocolName := BrowserTerminalWebSocketSubprotocol
		if compare {
			protocolName = "paperboat.browser-config-compare.e2ee.v1"
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		headers := http.Header{"Authorization": []string{"Bearer fixture"}}
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(origin.URL, "http"), &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{protocolName}})
		if err != nil {
			cancel()
			origin.Close()
			t.Fatalf("comparison=%t protocol negotiation failed: %v", compare, err)
		}
		if conn.Subprotocol() != protocolName {
			conn.CloseNow()
			cancel()
			origin.Close()
			t.Fatalf("comparison=%t negotiated wrong protocol", compare)
		}
		kind, identity, err := conn.Read(ctx)
		conn.CloseNow()
		cancel()
		origin.Close()
		if err != nil || kind != websocket.MessageBinary || len(identity) == 0 {
			t.Fatalf("comparison=%t did not deliver identity", compare)
		}
	}
}
