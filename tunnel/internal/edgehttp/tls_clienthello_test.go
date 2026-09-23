package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

type helloTestConn struct {
	input  *bytes.Reader
	output bytes.Buffer
	chunk  int
}

func (c *helloTestConn) Read(p []byte) (int, error) {
	if c.chunk > 0 && len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.input.Read(p)
}
func (c *helloTestConn) Write(p []byte) (int, error) { return c.output.Write(p) }
func (c *helloTestConn) Close() error                { return nil }
func (c *helloTestConn) CloseWrite() error           { return nil }
func (c *helloTestConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4443}
}
func (c *helloTestConn) RemoteAddr() net.Addr             { return c.LocalAddr() }
func (c *helloTestConn) SetDeadline(time.Time) error      { return nil }
func (c *helloTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *helloTestConn) SetWriteDeadline(time.Time) error { return nil }

func testClientHello(t *testing.T, hostname string) []byte {
	t.Helper()
	conn := &helloTestConn{input: bytes.NewReader(nil)}
	client := tls.Client(conn, &tls.Config{ServerName: hostname, InsecureSkipVerify: true}) // only emits a ClientHello; no peer is trusted
	_ = client.HandshakeContext(t.Context())
	raw := conn.output.Bytes()
	if len(raw) < 5 {
		t.Fatal("client did not emit ClientHello")
	}
	size := int(binary.BigEndian.Uint16(raw[3:5])) + 5
	return bytes.Clone(raw[:size])
}

func TestTLSClientHelloFragmentationAndExactReplay(t *testing.T) {
	original := testClientHello(t, "APP.customer.test")
	// Split a single ClientHello across TLS records, including its handshake header.
	var fragmented []byte
	payload := original[5:]
	for len(payload) > 0 {
		n := 17
		if n > len(payload) {
			n = len(payload)
		}
		fragmented = append(fragmented, 22, 3, 1, byte(n>>8), byte(n))
		fragmented = append(fragmented, payload[:n]...)
		payload = payload[n:]
	}
	for _, wire := range [][]byte{original, fragmented} {
		conn := &helloTestConn{input: bytes.NewReader(wire), chunk: 1}
		replay, host, err := inspectTLSClientHello(t.Context(), conn)
		if err != nil || host != "app.customer.test" {
			t.Fatalf("host %q err %v", host, err)
		}
		got, err := io.ReadAll(replay)
		if err != nil || !bytes.Equal(got, wire) {
			t.Fatal("inspected bytes changed")
		}
		if conn.output.Len() != 0 {
			t.Fatal("inspection wrote TLS bytes to peer")
		}
		if replay.LocalAddr().String() != conn.LocalAddr().String() {
			t.Fatal("lost listener identity")
		}
		if err := replay.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTLSClientHelloRejectsUnsupportedAndBoundedInput(t *testing.T) {
	valid := testClientHello(t, "app.customer.test")
	oversized := append([]byte{22, 3, 1, 255, 255}, bytes.Repeat([]byte{0}, maximumClientHelloBytes)...)
	for name, wire := range map[string][]byte{"absent": testClientHello(t, ""), "ip": testClientHello(t, "127.0.0.1"), "malformed": []byte("GET / HTTP/1.1\r\n"), "truncated": valid[:len(valid)-1], "oversized": oversized, "trailing-dot": testClientHello(t, "bad..test")} {
		t.Run(name, func(t *testing.T) {
			conn := &helloTestConn{input: bytes.NewReader(wire)}
			if _, _, err := inspectTLSClientHello(t.Context(), conn); err == nil {
				t.Fatal("accepted invalid hello")
			}
			if conn.output.Len() != 0 {
				t.Fatal("rejection wrote TLS bytes")
			}
		})
	}
}

func TestTLSClientHelloCancellation(t *testing.T) {
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
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, _, err := inspectTLSClientHello(ctx, conn); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("inspection did not cancel")
	}
}

func FuzzTLSClientHello(f *testing.F) {
	f.Add([]byte{22, 3, 1, 0, 0})
	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) > maximumClientHelloBytes+1 {
			return
		}
		conn := &helloTestConn{input: bytes.NewReader(wire)}
		replay, _, err := inspectTLSClientHello(t.Context(), conn)
		if conn.output.Len() != 0 {
			t.Fatal("parser wrote to peer")
		}
		if err == nil {
			got, _ := io.ReadAll(replay)
			if !bytes.Equal(got, wire) {
				t.Fatal("replay changed bytes")
			}
		}
	})
}
