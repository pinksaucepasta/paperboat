//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func gatewayCRLExchange(t *testing.T, request string, lookup func(string) []byte) (reply string, replayed []byte, served bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(client, request); err != nil {
		t.Fatal(err)
	}
	conn, served := serveGatewayCRL(server, lookup)
	if served {
		data, _ := io.ReadAll(client)
		return string(data), nil, true
	}
	defer conn.Close()
	data := make([]byte, len(request))
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Fatalf("replayed request: %v", err)
	}
	return "", data, false
}

func TestGatewayAnswersItsCRLBeforeAttributionAndReplaysEverythingElse(t *testing.T) {
	path := splitdns.CRLPathPrefix + strings.Repeat("ab", 32) + ".crl"
	der := []byte("crl-bytes")
	lookup := func(p string) []byte {
		if p == path {
			return der
		}
		return nil
	}
	reply, _, served := gatewayCRLExchange(t, "GET "+path+" HTTP/1.1\r\nHost: 127.100.0.1:80\r\n\r\n", lookup)
	if !served || !strings.HasPrefix(reply, "HTTP/1.1 200 OK\r\n") || !strings.Contains(reply, "Content-Type: application/pkix-crl\r\n") || !strings.HasSuffix(reply, "\r\n\r\n"+string(der)) {
		t.Fatalf("GET reply served=%v %q", served, reply)
	}
	reply, _, served = gatewayCRLExchange(t, "HEAD "+path+" HTTP/1.1\r\n\r\n", lookup)
	if !served || strings.HasSuffix(reply, string(der)) || !strings.Contains(reply, "Content-Length: 9\r\n") {
		t.Fatalf("HEAD reply served=%v %q", served, reply)
	}
	// Everything else is untouched: another path, an unknown CRL path, a query, a POST, a request that is not HTTP.
	for _, request := range []string{
		"GET /index.html HTTP/1.1\r\nHost: 127.100.0.1\r\n\r\n",
		"GET " + splitdns.CRLPathPrefix + strings.Repeat("cd", 32) + ".crl HTTP/1.1\r\n\r\n",
		"GET " + path + "?x=1 HTTP/1.1\r\n\r\n",
		"POST " + path + " HTTP/1.1\r\nContent-Length: 4\r\n\r\nbody",
		"\x16\x03\x01 not http at all\n",
	} {
		_, replayed, served := gatewayCRLExchange(t, request, lookup)
		if served || !bytes.Equal(replayed, []byte(request)) {
			t.Fatalf("request %q served=%v replayed %q", request, served, replayed)
		}
	}
}
