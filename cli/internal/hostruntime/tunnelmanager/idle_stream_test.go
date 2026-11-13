package tunnelmanager

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

// A quiet WebSocket (a server without pings) stays open for upgradedStreamIdle once upgraded, not the
// route's 90 s HTTP idle bound.
func TestUpgradedStreamGetsTheLongIdleBound(t *testing.T) {
	edge, edgePeer := net.Pipe()
	origin, originPeer := net.Pipe()
	stream := &idleOriginStream{ReadWriteCloser: edge, idle: 90 * time.Second}
	request, _ := http.NewRequest(http.MethodGet, "http://app.example.test/ws", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response := &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, Body: origin}
	done := make(chan error, 1)
	go func() {
		done <- writeOriginResponse(context.Background(), stream, bufio.NewReader(strings.NewReader("")), request, response)
	}()
	if _, err := http.ReadResponse(bufio.NewReader(edgePeer), request); err != nil {
		t.Fatal(err)
	}
	_ = edgePeer.Close()
	_ = originPeer.Close()
	<-done
	if stream.idle != upgradedStreamIdle {
		t.Fatalf("upgraded stream idle = %s, want %s", stream.idle, upgradedStreamIdle)
	}
}

// A quiet TCP stream is not cut at the route's HTTP idle bound (F21-37: the deployed edge closed a
// quiet public TCP stream after 90 s, not its own 1 h); an HTTP stream still is.
func TestQuietTCPStreamOutlivesTheRouteIdleBound(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls", "tcp_private"} {
		edge, peer := net.Pipe()
		defer peer.Close()
		stream := boundedOriginStream(edge, hoststate.TunnelConfigRoute{Protocol: protocol, IdleTimeoutMs: 50})
		read := make(chan error, 1)
		go func() { _, err := stream.Read(make([]byte, 1)); read <- err }()
		select {
		case err := <-read:
			t.Fatalf("%s stream ended while quiet: %v", protocol, err)
		case <-time.After(300 * time.Millisecond):
		}
	}
	edge, peer := net.Pipe()
	defer peer.Close()
	stream := boundedOriginStream(edge, hoststate.TunnelConfigRoute{Protocol: "http", IdleTimeoutMs: 50})
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("quiet http stream outlived the route idle bound")
	}
}
