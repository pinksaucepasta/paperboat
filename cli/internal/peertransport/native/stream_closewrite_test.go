package native

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"closewrite-test"}, InsecureSkipVerify: true}
}

// The FIN towards a client that already stopped reading is moot: quic-go's Close fails with a plain error for a send
// side the peer canceled, and streamConn.CloseWrite names it net.ErrClosed so the SSH bridge sees the client's read side
// as done.
func TestCloseWriteAfterThePeerStoppedReadingIsClosed(t *testing.T) {
	for _, accept := range []bool{false, true} {
		name := map[bool]string{false: "dial path (streamConn)", true: "accept path (routedConn)"}[accept]
		t.Run(name, func(t *testing.T) { closeWriteAfterThePeerStoppedReading(t, accept) })
	}
}

func closeWriteAfterThePeerStoppedReading(t *testing.T, accept bool) {
	config := selfSignedTLS(t)
	listener, err := quic.ListenAddr("127.0.0.1:0", config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	type accepted struct {
		stream *quic.Stream
		err    error
	}
	got := make(chan accepted, 1)
	go func() {
		connection, acceptErr := listener.Accept(context.Background())
		if acceptErr != nil {
			got <- accepted{err: acceptErr}
			return
		}
		stream, acceptErr := connection.AcceptStream(context.Background())
		got <- accepted{stream: stream, err: acceptErr}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := quic.DialAddr(ctx, listener.Addr().String(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseWithError(0, "")
	clientStream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientStream.Write([]byte("x")); err != nil { // the peer learns of the stream
		t.Fatal(err)
	}
	server := <-got
	if server.err != nil {
		t.Fatal(server.err)
	}
	var conn interface{ CloseWrite() error } = &streamConn{Stream: server.stream, local: directAddr("local"), remote: directAddr("peer")}
	if accept {
		conn = &routedConn{RoutedStream: &peerquic.RoutedStream{Stream: server.stream}, local: directAddr("local"), remote: directAddr("peer")}
	}
	// The client stops reading: STOP_SENDING cancels the server's send side.
	clientStream.CancelRead(0)
	for deadline := time.Now().Add(5 * time.Second); server.stream.Context().Err() == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the server's send side was never canceled by the peer")
		}
	}
	err = conn.CloseWrite()
	if err == nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("CloseWrite after the peer stopped reading = %v, want net.ErrClosed", err)
	}
}

// The mapping is narrow: a send side canceled locally (CancelWrite), or a connection that is gone, is not "the peer
// stopped reading": a failed FIN there stays an error, on the accept path too.
func TestCloseWriteFailureOtherThanThePeerStoppingReadingStaysAnError(t *testing.T) {
	for _, accept := range []bool{false, true} {
		stream, _, connection := openedServerStream(t)
		var conn interface{ CloseWrite() error } = &streamConn{Stream: stream, local: directAddr("local"), remote: directAddr("peer")}
		if accept {
			conn = &routedConn{RoutedStream: &peerquic.RoutedStream{Stream: stream}, local: directAddr("local"), remote: directAddr("peer")}
		}
		stream.CancelWrite(7) // our own cancel: Close then fails with the plain error, but not because the peer stopped reading
		err := conn.CloseWrite()
		if err == nil || errors.Is(err, net.ErrClosed) {
			t.Fatalf("CloseWrite after a local cancel = %v (accept path %v), want a plain error", err, accept)
		}
		connection.CloseWithError(0, "")
	}
}

// openedServerStream returns the server's side of a fresh quic-go stream and the client's stream and connection.
func openedServerStream(t *testing.T) (*quic.Stream, *quic.Stream, *quic.Conn) {
	t.Helper()
	config := selfSignedTLS(t)
	listener, err := quic.ListenAddr("127.0.0.1:0", config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	type accepted struct {
		stream *quic.Stream
		err    error
	}
	got := make(chan accepted, 1)
	go func() {
		connection, acceptErr := listener.Accept(context.Background())
		if acceptErr != nil {
			got <- accepted{err: acceptErr}
			return
		}
		stream, acceptErr := connection.AcceptStream(context.Background())
		got <- accepted{stream: stream, err: acceptErr}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := quic.DialAddr(ctx, listener.Addr().String(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.CloseWithError(0, "") })
	clientStream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientStream.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	server := <-got
	if server.err != nil {
		t.Fatal(server.err)
	}
	return server.stream, clientStream, connection
}
