package httptransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type noProxy struct{}

func (noProxy) Snapshot(context.Context) (ProxySnapshot, error) { return ProxySnapshot{}, nil }

// lossyDNS answers A queries for every name with 127.0.0.1 and AAAA with no records, but drops the first query
// it receives, as a lossy network or Docker's resolver does. The client's resolver retries after its timeout.
func lossyDNS(t *testing.T) (address string, dropped *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	dropped = &atomic.Int32{}
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			if dropped.CompareAndSwap(0, 1) {
				continue
			}
			var parser dnsmessage.Parser
			header, err := parser.Start(buffer[:n])
			if err != nil {
				continue
			}
			question, err := parser.Question()
			if err != nil {
				continue
			}
			builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true})
			builder.EnableCompression()
			_ = builder.StartQuestions()
			_ = builder.Question(question)
			_ = builder.StartAnswers()
			if question.Type == dnsmessage.TypeA {
				_ = builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 60},
					dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}})
			}
			if answer, err := builder.Finish(); err == nil {
				_, _ = conn.WriteTo(answer, peer)
			}
		}
	}()
	return conn.LocalAddr().String(), dropped
}

// A lost DNS packet costs one resolver retry (5 s by default); the dial bound covers lookup and connect, so the
// request still succeeds. With the former 5 s bound this failed as "Paperboat is unreachable".
func TestRequestSucceedsWhenTheFirstDNSQueryIsLost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	dns, dropped := lossyDNS(t)
	config := DevelopmentConfig()
	config.ProxySource = noProxy{}
	config.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", dns)
	}}
	transport, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://api.example.test:"+port+"/", nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatalf("request failed after one lost DNS query: %v", err)
	}
	defer response.Body.Close()
	if body, _ := io.ReadAll(response.Body); string(body) != "ok" || dropped.Load() != 1 {
		t.Fatalf("body %q, dropped %d", body, dropped.Load())
	}
}

// A replayable request is tried once more when the name lookup or connect failed (nothing reached the server);
// a POST without an idempotency key is not, and a keyed POST replays its exact body.
func TestReplayableRequestsRetryOnceAfterALookupOrConnectFailure(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, method, key, body string
		fail                    error
		wantOK                  bool
		wantDials               int32
	}{
		{"GET after a DNS timeout", http.MethodGet, "", "", &net.DNSError{Err: "i/o timeout", Name: "api.example.test", IsTimeout: true}, true, 2},
		{"GET after a refused connect", http.MethodGet, "", "", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true, 2},
		{"keyed POST replays its body", http.MethodPost, "op-1", "payload", &net.DNSError{Err: "no such host", Name: "api.example.test"}, true, 2},
		{"POST without a key is not retried", http.MethodPost, "", "payload", &net.DNSError{Err: "no such host", Name: "api.example.test"}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies = nil
			var dials atomic.Int32
			config := DevelopmentConfig()
			config.ProxySource = noProxy{}
			config.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					return nil, tc.fail
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			transport, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			request, _ := http.NewRequest(tc.method, "http://api.example.test/", body)
			if tc.key != "" {
				request.Header.Set("Idempotency-Key", tc.key)
			}
			response, err := (&http.Client{Transport: transport}).Do(request)
			if response != nil {
				response.Body.Close()
			}
			if (err == nil) != tc.wantOK || dials.Load() != tc.wantDials {
				t.Fatalf("err=%v dials=%d, want ok=%t dials=%d", err, dials.Load(), tc.wantOK, tc.wantDials)
			}
			if tc.wantOK && tc.body != "" && fmt.Sprint(bodies) != "["+tc.body+"]" {
				t.Fatalf("server saw bodies %q", bodies)
			}
		})
	}
}
