package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestAdmittedTransferHTTPConnectionOutlivesCredentialAndRevokesBlockedIO(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	factory := handler.config.Authorizer
	expires := time.Now().Add(100 * time.Millisecond)
	revoked := make(chan struct{})
	flag := new(atomic.Bool)
	handler.config.Authorizer = func(token string) (Authorizer, error) {
		if !time.Now().Before(expires) {
			return nil, &auth.Error{Code: auth.Expired}
		}
		original, err := factory(token)
		if err != nil {
			return nil, err
		}
		return authorizerFunc(func(ctx context.Context, f protocol.Frame) (Authorization, error) {
			a, err := original.Authorize(ctx, f)
			a.ExpiresAt = expires
			a.Revoked = flag
			a.RevokedSignal = revoked
			return a, err
		}), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held := make(chan struct{})
	application := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Hold") == "1" {
			close(held)
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	})
	var workers sync.WaitGroup
	var mu sync.Mutex
	var pipes []net.Conn
	// Each transport gets exactly one connection. After revocation cuts it, the
	// HTTP transport retries the idempotent held request on a new connection;
	// that retry must fail instead of reaching the fixture's hold handler,
	// which bypasses authentication and would block until the client timeout.
	transport := func() *http.Transport {
		dialed := false
		return &http.Transport{MaxConnsPerHost: 1, DialContext: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			if dialed {
				mu.Unlock()
				return nil, errors.New("fixture allows one connection per transport")
			}
			dialed = true
			mu.Unlock()
			client, server := net.Pipe()
			mu.Lock()
			pipes = append(pipes, client, server)
			mu.Unlock()
			workers.Add(1)
			go func() { defer workers.Done(); _ = ServeHTTPConnection(ctx, server, application) }()
			return client, nil
		}}
	}
	first, fresh := transport(), transport()
	defer func() {
		cancel()
		first.CloseIdleConnections()
		fresh.CloseIdleConnections()
		mu.Lock()
		for _, p := range pipes {
			p.Close()
		}
		mu.Unlock()
		workers.Wait()
	}()
	client := &http.Client{Transport: first, Timeout: 2 * time.Second}
	request := func(token string) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://machine.example.test/v1/file-transfers/policy", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	status := func(c *http.Client, token string) int {
		t.Helper()
		r, err := c.Do(request(token))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.StatusCode
	}
	if got := status(client, "token"); got != 200 {
		t.Fatal("initial admission", got)
	}
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if got := status(client, "token"); got != 200 {
		t.Fatal("expiry ended admitted transfer connection", got)
	}
	if got := status(&http.Client{Transport: fresh, Timeout: time.Second}, "token"); got != 401 {
		t.Fatal("expired credential admitted new connection", got)
	}
	if got := status(client, "other"); got != 401 {
		t.Fatal("connection accepted credential substitution", got)
	}
	blocked := make(chan error, 1)
	go func() {
		r := request("token")
		r.Header.Set("X-Fixture-Hold", "1")
		response, err := client.Do(r)
		if response != nil {
			response.Body.Close()
		}
		blocked <- err
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("request did not enter blocked I/O")
	}
	flag.Store(true)
	close(revoked)
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("revocation left HTTP connection live")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("revocation waited for request timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not interrupt HTTP connection")
	}
}
