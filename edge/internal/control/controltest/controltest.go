// Package controltest is a stand-in control plane for browser-terminal control tests, behind a TLS
// proxy that cuts held streams the way Caddy does when a reload's stream_close_delay ends:
// closeConnections (modules/caddyhttp/reverseproxy/streaming.go) writes a websocket Close frame with
// status 1001 (going away) to the client and closes both connections.
package controltest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Heartbeat is how often the stand-in sends "ok" (the server checks every 2 s).
const Heartbeat = 100 * time.Millisecond

// Request is one control request the stand-in received.
type Request struct {
	Ticket, Origin, Host, Resume string
}

// Plane admits one attachment, resumes it, and cuts or refuses on demand.
type Plane struct {
	URL    string
	Client *http.Client

	mu       sync.Mutex
	requests []Request
	refusal  string // close reason for every later request, as the server refusing it
	down     bool   // the proxy accepts nothing: a control-plane outage
	pairs    []net.Conn
	listener net.Listener
}

// Start runs the stand-in for the test's lifetime.
func Start(t *testing.T) *Plane {
	t.Helper()
	plane := &Plane{}
	backend := httptest.NewServer(http.HandlerFunc(plane.serve))
	t.Cleanup(backend.Close)
	certificate := httptest.NewTLSServer(http.NotFoundHandler()) // only its certificate and client
	t.Cleanup(certificate.Close)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", certificate.TLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	plane.listener, plane.URL, plane.Client = listener, "https://"+listener.Addr().String(), certificate.Client()
	go plane.proxy(backend.Listener.Addr().String())
	return plane
}

func (p *Plane) proxy(backend string) {
	for {
		front, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			_ = front.Close()
			continue
		}
		back, err := net.Dial("tcp", backend)
		if err != nil {
			_ = front.Close()
			continue
		}
		p.mu.Lock()
		p.pairs = append(p.pairs, front, back)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(back, front); _ = back.Close() }()
		go func() { _, _ = io.Copy(front, back); _ = front.Close() }()
	}
}

// Cut closes every held stream as a reload's stream_close_delay does: Close 1001 to the client, then
// both connections.
func (p *Plane) Cut() {
	p.mu.Lock()
	pairs := p.pairs
	p.pairs = nil
	p.mu.Unlock()
	for i := 0; i < len(pairs); i += 2 {
		_, _ = pairs[i].Write([]byte{0x88, 0x02, 0x03, 0xe9}) // FIN|Close, unmasked, status 1001
		_ = pairs[i].Close()
		_ = pairs[i+1].Close()
	}
}

// Refuse makes the server refuse every later request with reason (revoked, expired, ...): the
// answer a resume gets for a terminal revoked while its channel was cut. Held channels go on.
func (p *Plane) Refuse(reason string) {
	p.mu.Lock()
	p.refusal = reason
	p.mu.Unlock()
}

// Down makes the proxy accept and drop every connection: the control plane is unreachable.
func (p *Plane) Down() {
	p.mu.Lock()
	p.down = true
	p.mu.Unlock()
}

// Requests returns the control requests received, in order.
func (p *Plane) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.requests...)
}

func (p *Plane) serve(w http.ResponseWriter, r *http.Request) {
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	_, raw, err := connection.Read(r.Context())
	if err != nil {
		return
	}
	var request Request
	if json.Unmarshal(raw, &request) != nil {
		return
	}
	p.mu.Lock()
	p.requests = append(p.requests, request)
	refusal := p.refusal
	p.mu.Unlock()
	if refusal != "" {
		_ = connection.Close(websocket.StatusPolicyViolation, refusal)
		return
	}
	reply, _ := json.Marshal(map[string]string{"credential": "host-credential", "terminal_session_id": "term_1", "attachment_id": "attach_1"})
	if connection.Write(r.Context(), websocket.MessageText, reply) != nil {
		return
	}
	ticker := time.NewTicker(Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			err := connection.Write(ctx, websocket.MessageText, []byte("ok"))
			cancel()
			if err != nil {
				return
			}
		}
	}
}
