package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// dropAfterHandling applies the request, then closes the connection without a response (Lane S:
// POST /v1/teams/<team>/actions logged 200 while the CLI said unreachable).
func dropAfterHandling(t *testing.T, applied *atomic.Int32, failFirst int32) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if applied.Add(1) > failFirst {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAMutationWhoseResponseIsLostIsUncertain(t *testing.T) {
	var applied atomic.Int32
	server := dropAfterHandling(t, &applied, 100)
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	err := client.do(context.Background(), http.MethodPost, "/v1/teams/team_1/actions", map[string]string{"action": "x"}, nil)
	var lost *ResponseLostError
	if !errors.As(err, &lost) || applied.Load() != 1 {
		t.Fatalf("err=%v applied=%d", err, applied.Load())
	}
}

func TestAReadRetriesAfterATransportFailure(t *testing.T) {
	var calls atomic.Int32
	server := dropAfterHandling(t, &calls, 2)
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	var out map[string]any
	if err := client.do(context.Background(), http.MethodGet, "/v1/machines", nil, &out); err != nil || calls.Load() < 3 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	calls.Store(-10)
	err := client.do(context.Background(), http.MethodGet, "/v1/machines", nil, &out)
	var lost *ResponseLostError
	// net/http itself resends an idempotent request once on a reused connection that closed.
	if sent := calls.Load() + 10; err == nil || errors.As(err, &lost) || sent < readRetryAttempts || sent > 2*readRetryAttempts {
		t.Fatalf("bounded retry: err=%v calls=%d", err, calls.Load())
	}
}

// A token create whose response was lost is sent again with the same Idempotency-Key, so the
// server can answer with the same token and once-shown secret (F21-12).
func TestCreateAPITokenRetriesALostResponseWithTheSameKey(t *testing.T) {
	var applied atomic.Int32
	keys := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		if applied.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"api_token":{"id":"apitok_1","name":"ci"},"secret":"pbat_x"}}`))
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	out, err := client.CreateAPIToken(context.Background(), "personal", APITokenCreate{Name: "ci"})
	close(keys)
	var sent []string
	for k := range keys {
		sent = append(sent, k)
	}
	if err != nil || out.Secret != "pbat_x" || len(sent) != 2 || sent[0] == "" || sent[0] != sent[1] {
		t.Fatalf("out=%+v err=%v keys=%v", out, err, sent)
	}
}
