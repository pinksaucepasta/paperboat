package runtime

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A handover drain must not drop a client that reuses a keep-alive connection while the server drains: its request is
// answered and the answer ends the connection (a plain Shutdown closed the idle connection at once and the client saw
// a bare EOF). New connections are refused from the start of the drain; the whole drain stays inside its bound.
func TestDrainAnswersAKeepAliveClientAndEndsItsConnection(t *testing.T) {
	server, err := NewHTTPServer(HTTPServerSpec{Address: "127.0.0.1:19000", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.listen = func(string, string) (net.Listener, error) { return listener, nil }
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	reader := bufio.NewReader(connection)
	get := func() (*http.Response, error) {
		if _, err := io.WriteString(connection, "GET / HTTP/1.1\r\nHost: preview.example.test\r\n\r\n"); err != nil {
			return nil, err
		}
		response, err := http.ReadResponse(reader, nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		return response, err
	}
	if first, err := get(); err != nil || first.StatusCode != http.StatusOK || first.Close {
		t.Fatalf("first request on a live server: %v %+v", err, first)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	drained := make(chan error, 1)
	go func() { drained <- server.Drain(ctx) }()
	time.Sleep(200 * time.Millisecond) // the drain is under way and the connection sits idle in its grace

	second, err := get()
	if err != nil || second.StatusCode != http.StatusOK || !second.Close {
		t.Fatalf("a keep-alive request during the drain: %v %+v (want 200 and Connection: close)", err, second)
	}
	if extra, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		extra.Close()
		t.Fatal("a new connection was accepted during the drain")
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the drain did not finish inside its bound")
	}
	if took := time.Since(started); took > httpDrainGrace+2*time.Second {
		t.Fatalf("the drain took %s, longer than its grace %s", took, httpDrainGrace)
	}
}
