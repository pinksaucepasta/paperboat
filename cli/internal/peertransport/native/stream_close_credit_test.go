package native

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

// F21-164: closing a stream the peer is still sending on must give its unread bytes back to the shared connection window.
// Before, Close sent only the FIN; 10 terminals closed mid-burst filled the window and every other stream on the peer
// stalled. Here one closed stream holds the whole window; another stream must still deliver.
func TestCloseReturnsUnreadBytesToTheConnectionWindow(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "streamConn", true: "routedConn"}[accept], func(t *testing.T) { closeReturnsCredit(t, accept) })
	}
}

func closeReturnsCredit(t *testing.T, accept bool) {
	const window = 1 << 20
	config := &quic.Config{InitialStreamReceiveWindow: window, MaxStreamReceiveWindow: window,
		InitialConnectionReceiveWindow: window, MaxConnectionReceiveWindow: window}
	tlsConfig := selfSignedTLS(t)
	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sender := make(chan *quic.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept(ctx)
		if acceptErr == nil {
			sender <- connection
		}
		close(sender)
	}()
	receiver, err := quic.DialAddr(ctx, listener.Addr().String(), tlsConfig, config)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.CloseWithError(0, "")
	server := <-sender
	if server == nil {
		t.Fatal("no connection accepted")
	}
	wrap := func(stream *quic.Stream) net.Conn {
		if accept {
			return &routedConn{RoutedStream: &peerquic.RoutedStream{Stream: stream}, local: directAddr("l"), remote: directAddr("r")}
		}
		return &streamConn{Stream: stream, local: directAddr("l"), remote: directAddr("r")}
	}
	// The burst: the server sends more than the window on a stream the receiver never reads, then closes it.
	burst, err := server.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = burst.Write(make([]byte, 2*window)) }()
	abandoned, err := receiver.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the window fills with unread burst bytes
	if err := wrap(abandoned).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Another terminal on the same association still gets its output.
	other, err := server.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = other.Write(make([]byte, window/2)); _ = other.Close() }()
	survivor, err := receiver.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("the other stream never arrived: %v", err)
	}
	_ = survivor.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := io.Copy(io.Discard, survivor); err != nil || n != window/2 {
		t.Fatalf("other stream after the close: read %d of %d bytes, err %v (connection window still held)", n, window/2, err)
	}
}
