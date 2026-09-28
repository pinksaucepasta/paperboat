package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

const (
	BrowserTerminalWebSocketSubprotocol = "paperboat.browser-terminal.e2ee.v1"
	BrowserTerminalTLSALPN              = "paperboat.browser-terminal.e2ee.v1"
	defaultBrowserTerminalMaxMessage    = 1 << 20
	maxBrowserTerminalMaxMessage        = 4 << 20
	browserTerminalHandshakeTimeout     = 10 * time.Second
	browserTerminalWriteTimeout         = 10 * time.Second
	browserTerminalCloseTimeout         = time.Second
	maxBrowserClientCertificateAge      = 10 * time.Minute
)

var ErrBrowserTerminalIdentityUnavailable = errors.New("browser terminal identity unavailable")

// BrowserTerminalIdentity contains machine identity material which has
// already been verified by the runtime identity store. The private key is the
// machine's existing QUIC key; this transport does not create another machine
// identity.
type BrowserTerminalIdentity struct {
	Certificate    []byte
	RootKeyID      string
	TLSCertificate tls.Certificate
}

type BrowserTerminalIdentityProvider func(context.Context) (BrowserTerminalIdentity, error)

// BrowserTerminalCredentialKey is implemented by the operation authorizer
// used for the dedicated E2EE browser endpoint. It verifies the signed
// browser-terminal JWT before TLS and returns its canonical SPKI pin.
type BrowserTerminalCredentialKey interface {
	BrowserTerminalPublicKeySHA256(context.Context) (string, error)
}

type BrowserTerminalWebSocketHandlerConfig struct {
	Server          *Server
	Authorizer      AuthorizerFactory
	Identity        BrowserTerminalIdentityProvider
	OriginPatterns  []string
	MaxConnections  int
	MaxMessageBytes int64
	Limiter         *ConnectionLimiter
}

type BrowserTerminalWebSocketHandler struct {
	config  BrowserTerminalWebSocketHandlerConfig
	limiter *ConnectionLimiter
}

func NewBrowserTerminalWebSocketHandler(config BrowserTerminalWebSocketHandlerConfig) (*BrowserTerminalWebSocketHandler, error) {
	if config.MaxConnections == 0 {
		config.MaxConnections = 128
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = defaultBrowserTerminalMaxMessage
	}
	if config.Limiter == nil {
		var err error
		config.Limiter, err = NewConnectionLimiter(config.MaxConnections)
		if err != nil {
			return nil, err
		}
	}
	if config.Server == nil || config.Authorizer == nil || config.Identity == nil || config.MaxConnections < 1 || config.MaxMessageBytes < 1 || config.MaxMessageBytes > maxBrowserTerminalMaxMessage {
		return nil, ErrInvalidConfiguration
	}
	return &BrowserTerminalWebSocketHandler{config: config, limiter: config.Limiter}, nil
}

func (h *BrowserTerminalWebSocketHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	token, ok := bearerToken(request.Header.Values("Authorization"))
	if !ok {
		http.Error(writer, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	authorizer, err := h.config.Authorizer(token)
	if err != nil || authorizer == nil {
		http.Error(writer, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	keyAuthorizer, ok := authorizer.(BrowserTerminalCredentialKey)
	if !ok {
		http.Error(writer, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	pinnedDigest, err := keyAuthorizer.BrowserTerminalPublicKeySHA256(request.Context())
	if err != nil || !validBrowserTerminalDigest(pinnedDigest) {
		http.Error(writer, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	identity, err := h.config.Identity(request.Context())
	if err != nil {
		http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	identityEnvelope, err := encodeBrowserTerminalIdentityEnvelope(identity)
	if err != nil {
		http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if !h.limiter.acquire() {
		writer.Header().Set("Retry-After", "1")
		http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer h.limiter.release()

	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols:    []string{BrowserTerminalWebSocketSubprotocol},
		OriginPatterns:  append([]string(nil), h.config.OriginPatterns...),
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if connection.Subprotocol() != BrowserTerminalWebSocketSubprotocol {
		_ = connection.Close(websocket.StatusPolicyViolation, "subprotocol_required")
		return
	}
	connection.SetReadLimit(h.config.MaxMessageBytes)
	identityCtx, cancelIdentity := context.WithTimeout(request.Context(), browserTerminalWriteTimeout)
	err = connection.Write(identityCtx, websocket.MessageBinary, identityEnvelope)
	cancelIdentity()
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "identity_unavailable")
		return
	}

	stream := newBrowserTerminalWebSocketStream(request.Context(), connection)
	tlsConnection := tls.Server(stream, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{BrowserTerminalTLSALPN},
		Certificates: []tls.Certificate{identity.TLSCertificate},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCertificates [][]byte, _ [][]*x509.Certificate) error {
			return verifyBrowserTerminalClientCertificate(rawCertificates, pinnedDigest, time.Now().UTC())
		},
	})
	_ = stream.SetDeadline(time.Now().Add(browserTerminalHandshakeTimeout))
	handshakeCtx, cancelHandshake := context.WithTimeout(request.Context(), browserTerminalHandshakeTimeout)
	err = tlsConnection.HandshakeContext(handshakeCtx)
	cancelHandshake()
	_ = stream.SetDeadline(time.Time{})
	if err != nil || tlsConnection.ConnectionState().NegotiatedProtocol != BrowserTerminalTLSALPN {
		_ = stream.Close()
		_ = connection.Close(websocket.StatusPolicyViolation, "client_certificate_required")
		return
	}

	application := newBrowserTerminalApplicationConnection(request.Context(), connection, tlsConnection)
	_ = h.config.Server.ServeAuthenticated(application, authorizer)
}

type browserTerminalIdentityDocument struct {
	Certificate string `json:"certificate"`
	RootKeyID   string `json:"root_key_id"`
}

func encodeBrowserTerminalIdentityEnvelope(identity BrowserTerminalIdentity) ([]byte, error) {
	if len(identity.Certificate) == 0 || len(identity.Certificate) > 1024 || !validBrowserTerminalRootKeyID(identity.RootKeyID) || len(identity.TLSCertificate.Certificate) != 1 {
		return nil, ErrBrowserTerminalIdentityUnavailable
	}
	endpoint, err := endpointidentity.Parse(identity.Certificate)
	if err != nil {
		return nil, errors.Join(ErrBrowserTerminalIdentityUnavailable, err)
	}
	leaf := identity.TLSCertificate.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(identity.TLSCertificate.Certificate[0])
		if err != nil {
			return nil, errors.Join(ErrBrowserTerminalIdentityUnavailable, err)
		}
	}
	public, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || len(public) != ed25519.PublicKeySize || subtle.ConstantTimeCompare(public, endpoint.Claims.QUICPublicKey) != 1 || identity.TLSCertificate.PrivateKey == nil {
		return nil, ErrBrowserTerminalIdentityUnavailable
	}
	encoded, err := json.Marshal(browserTerminalIdentityDocument{Certificate: base64.RawURLEncoding.EncodeToString(identity.Certificate), RootKeyID: identity.RootKeyID})
	if err != nil || len(encoded) > 2<<10 {
		return nil, ErrBrowserTerminalIdentityUnavailable
	}
	return encoded, nil
}

func validBrowserTerminalRootKeyID(value string) bool {
	if len(value) != 68 || !strings.HasPrefix(value, "aek_") {
		return false
	}
	for _, char := range value[4:] {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validBrowserTerminalDigest(value string) bool {
	if len(value) != 43 {
		return false
	}
	digest, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(digest) == sha256.Size && base64.RawURLEncoding.EncodeToString(digest) == value
}

func verifyBrowserTerminalClientCertificate(rawCertificates [][]byte, pinnedDigest string, now time.Time) error {
	if len(rawCertificates) != 1 || !validBrowserTerminalDigest(pinnedDigest) {
		return errors.New("browser TLS client certificate is missing or invalid")
	}
	certificate, err := x509.ParseCertificate(rawCertificates[0])
	if err != nil {
		return errors.New("browser TLS client certificate is malformed")
	}
	public, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || len(public) != ed25519.PublicKeySize || certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 || !hasClientAuthUsage(certificate.ExtKeyUsage) || !certificate.NotBefore.Before(now) || !now.Before(certificate.NotAfter) || certificate.NotAfter.Sub(certificate.NotBefore) > maxBrowserClientCertificateAge || !bytes.Equal(certificate.RawSubject, certificate.RawIssuer) {
		return errors.New("browser TLS client certificate does not meet the key policy")
	}
	if err := certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature); err != nil {
		return errors.New("browser TLS client certificate is not self-signed")
	}
	digest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	encoded := base64.RawURLEncoding.EncodeToString(digest[:])
	if subtle.ConstantTimeCompare([]byte(encoded), []byte(pinnedDigest)) != 1 {
		return errors.New("browser TLS client certificate does not match its grant")
	}
	return nil
}

func hasClientAuthUsage(usages []x509.ExtKeyUsage) bool {
	for _, usage := range usages {
		if usage == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

type browserTerminalWebSocketStream struct {
	connection *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	readUntil  time.Time
	writeUntil time.Time
	readMu     sync.Mutex
	writeMu    sync.Mutex
	buffer     []byte
	closed     atomic.Bool
}

func newBrowserTerminalWebSocketStream(parent context.Context, connection *websocket.Conn) *browserTerminalWebSocketStream {
	ctx, cancel := context.WithCancel(parent)
	return &browserTerminalWebSocketStream{connection: connection, ctx: ctx, cancel: cancel}
}

func (c *browserTerminalWebSocketStream) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.buffer) == 0 {
		ctx, cancel := c.operationContext(true)
		messageType, data, err := c.connection.Read(ctx)
		cancel()
		if err != nil {
			return 0, classifyWebSocketRead(err)
		}
		if messageType != websocket.MessageBinary || len(data) == 0 {
			return 0, errors.New("browser TLS stream requires non-empty binary WebSocket messages")
		}
		c.buffer = data
	}
	n := copy(buffer, c.buffer)
	c.buffer = c.buffer[n:]
	return n, nil
}

func (c *browserTerminalWebSocketStream) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	ctx, cancel := c.operationContext(false)
	defer cancel()
	if err := c.connection.Write(ctx, websocket.MessageBinary, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (c *browserTerminalWebSocketStream) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.cancel()
	return nil
}

func (c *browserTerminalWebSocketStream) LocalAddr() net.Addr {
	return browserTerminalAddr("browser-terminal-local")
}
func (c *browserTerminalWebSocketStream) RemoteAddr() net.Addr {
	return browserTerminalAddr("browser-terminal-peer")
}

func (c *browserTerminalWebSocketStream) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readUntil, c.writeUntil = deadline, deadline
	c.mu.Unlock()
	return nil
}

func (c *browserTerminalWebSocketStream) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readUntil = deadline
	c.mu.Unlock()
	return nil
}

func (c *browserTerminalWebSocketStream) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeUntil = deadline
	c.mu.Unlock()
	return nil
}

func (c *browserTerminalWebSocketStream) operationContext(read bool) (context.Context, context.CancelFunc) {
	c.mu.Lock()
	deadline := c.writeUntil
	if read {
		deadline = c.readUntil
	}
	c.mu.Unlock()
	if read {
		if deadline.IsZero() {
			return context.WithCancel(c.ctx)
		}
		return context.WithDeadline(c.ctx, deadline)
	}
	writeLimit := time.Now().Add(browserTerminalWriteTimeout)
	if deadline.IsZero() || writeLimit.Before(deadline) {
		deadline = writeLimit
	}
	return context.WithDeadline(c.ctx, deadline)
}

type browserTerminalAddr string

func (a browserTerminalAddr) Network() string { return "websocket" }
func (a browserTerminalAddr) String() string  { return string(a) }

type browserTerminalApplicationConnection struct {
	connection *websocket.Conn
	stream     *tls.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	readMu     sync.Mutex
	writeMu    sync.Mutex
	closeOnce  sync.Once
	revoked    atomic.Bool
}

func newBrowserTerminalApplicationConnection(parent context.Context, websocketConnection *websocket.Conn, tlsConnection *tls.Conn) *browserTerminalApplicationConnection {
	ctx, cancel := context.WithCancel(parent)
	connection := &browserTerminalApplicationConnection{connection: websocketConnection, stream: tlsConnection, ctx: ctx, cancel: cancel}
	context.AfterFunc(ctx, func() { connection.revoked.Store(true) })
	return connection
}

func (c *browserTerminalApplicationConnection) RevocationFlag() *atomic.Bool { return &c.revoked }

func (c *browserTerminalApplicationConnection) Read([]byte) (int, error) {
	return 0, errors.New("browser terminal uses encrypted application records")
}

func (c *browserTerminalApplicationConnection) Write(payload []byte) (int, error) {
	if err := c.writeApplicationRecord(1, payload, protocol.MaxStructuredFrame); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (c *browserTerminalApplicationConnection) ReadApplication() (protocol.Frame, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	var header [5]byte
	if _, err := io.ReadFull(c.stream, header[:]); err != nil {
		return protocol.Frame{}, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length == 0 {
		return protocol.Frame{}, nil, &protocol.Error{Code: protocol.Malformed, Cause: errors.New("empty browser application record")}
	}
	limit := uint32(protocol.MaxStructuredFrame)
	if header[0] == 2 {
		limit = uint32(protocol.MaxBinaryFrame)
	} else if header[0] != 1 {
		return protocol.Frame{}, nil, &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("unknown browser application record kind")}
	}
	if length > limit {
		return protocol.Frame{}, nil, &protocol.Error{Code: protocol.Oversized}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.stream, payload); err != nil {
		return protocol.Frame{}, nil, &protocol.Error{Code: protocol.Malformed, Cause: err}
	}
	if header[0] == 2 {
		return protocol.Frame{}, payload, nil
	}
	var framed bytes.Buffer
	var frameLength [4]byte
	binary.BigEndian.PutUint32(frameLength[:], uint32(len(payload)))
	framed.Write(frameLength[:])
	framed.Write(payload)
	frame, err := protocol.ReadFrame(&framed)
	if err != nil {
		return protocol.Frame{}, nil, err
	}
	return frame, nil, nil
}

func (c *browserTerminalApplicationConnection) WriteStructured(frame protocol.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return c.writeApplicationRecord(1, encoded, protocol.MaxStructuredFrame)
}

func (c *browserTerminalApplicationConnection) WriteBinary(frame protocol.BinaryFrame) error {
	encoded, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: frame.Channel, StreamID: 1, StartSequence: frame.StartSequence, Data: frame.Data}, nil)
	if err != nil {
		return err
	}
	return c.writeApplicationRecord(2, encoded, protocol.MaxBinaryFrame)
}

func (c *browserTerminalApplicationConnection) WriteTerminalOutput(streamID uint32, frame protocol.BinaryFrame) error {
	encoded, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: frame.Channel, StreamID: streamID, StartSequence: frame.StartSequence, Data: frame.Data}, nil)
	if err != nil {
		return err
	}
	return c.writeApplicationRecord(2, encoded, protocol.MaxBinaryFrame)
}

func (c *browserTerminalApplicationConnection) writeApplicationRecord(kind byte, payload []byte, limit int) error {
	if len(payload) == 0 || len(payload) > limit {
		return &protocol.Error{Code: protocol.Oversized}
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeBrowserTerminalTLS(c.stream, header[:]); err != nil {
		return err
	}
	return writeBrowserTerminalTLS(c.stream, payload)
}

func writeBrowserTerminalTLS(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := writer.Write(payload)
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

func (c *browserTerminalApplicationConnection) Close() error {
	return c.CloseProtocol(protocol.CloseNormal, "closed")
}

func (c *browserTerminalApplicationConnection) CloseProtocol(code int, reason string) error {
	var result error
	c.closeOnce.Do(func() {
		_ = c.stream.SetWriteDeadline(time.Now().Add(browserTerminalCloseTimeout))
		_ = c.stream.Close()
		result = c.connection.Close(websocket.StatusCode(code), reason)
		c.cancel()
	})
	return result
}
