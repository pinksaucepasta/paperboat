package tunnelmanager

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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
