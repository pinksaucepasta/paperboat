package datacarrier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"testing"
	"time"

	yamux "github.com/libp2p/go-yamux/v5"
)

// A carrier end built before DataCarrierMaxStreamWindow (window pinned at its initial size) and a
// current end must move more than 16 MiB in both directions without loss: each side's receive
// window is its own, so mixing versions changes speed only.
func TestCarrierStreamWindowInteroperatesWithPinnedWindowPeer(t *testing.T) {
	for _, pinnedIsClient := range []bool{true, false} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, _ := listener.Accept()
			accepted <- conn
		}()
		clientConn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		serverConn := <-accepted
		listener.Close()
		config := testCarrierConfig(4)
		config.KeepAliveInterval = time.Second
		pinned := func(conn net.Conn, client bool) Session {
			yamuxConfig := yamux.DefaultConfig()
			yamuxConfig.LogOutput = io.Discard
			yamuxConfig.MaxStreamWindowSize = config.StreamWindow
			yamuxConfig.InitialStreamWindowSize = config.StreamWindow
			var session *yamux.Session
			if client {
				session, err = yamux.Client(conn, yamuxConfig, nil)
			} else {
				session, err = yamux.Server(conn, yamuxConfig, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			return &yamuxSession{session: session}
		}
		var client, server Session
		if pinnedIsClient {
			client = pinned(clientConn, true)
			server, err = newYamuxSession(serverConn, config, false)
		} else {
			client, err = newYamuxSession(clientConn, config, true)
			server = pinned(serverConn, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 24<<20)
		for i := range payload {
			payload[i] = byte(i*7 + i>>13)
		}
		want := sha256.Sum256(payload)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		echoed := make(chan error, 1)
		go func() {
			stream, err := server.AcceptStream(ctx)
			if err != nil {
				echoed <- err
				return
			}
			defer stream.Close()
			_, err = io.CopyN(stream, stream, int64(len(payload))) // echo every byte back
			echoed <- err
		}()
		stream, err := client.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.Copy(stream, bytes.NewReader(payload)) }()
		got := sha256.New()
		n, err := io.CopyN(got, stream, int64(len(payload)))
		if err != nil || n != int64(len(payload)) || !bytes.Equal(got.Sum(nil), want[:]) {
			t.Fatalf("pinned client=%v: echoed %d of %d bytes: %v", pinnedIsClient, n, len(payload), err)
		}
		if err := <-echoed; err != nil {
			t.Fatalf("pinned client=%v: echo side: %v", pinnedIsClient, err)
		}
		stream.Close()
		client.Close()
		server.Close()
		cancel()
	}
}
