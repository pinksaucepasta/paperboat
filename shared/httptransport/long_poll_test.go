package httptransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The daemons never saw a healthy machine-events stream: the server holds a wait for 30 s and the
// shared transport's response-header timeout is also 30 s, so every wait failed just before its
// answer and inventory fell back to re-listing every 15 s (~355 machine listings a minute from 22
// machines). A request marked LongPoll waits past the header timeout; others still time out.
func TestLongPollRequestOutlivesTheResponseHeaderTimeout(t *testing.T) {
	const hold = 300 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(hold)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	config := DevelopmentConfig()
	config.ProxySource = StaticProxySource{}
	config.ResponseHeaderTimeout = hold / 3
	transport, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}
	get := func(ctx context.Context) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		return client.Do(request)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if response, err := get(ctx); err == nil {
		response.Body.Close()
		t.Fatal("an ordinary request waited past the response-header timeout")
	}
	for range 3 {
		response, err := get(LongPoll(ctx))
		if err != nil {
			t.Fatalf("long-poll request ended before the held answer: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("long poll status %d", response.StatusCode)
		}
	}
	if _, err := get(LongPoll(context.Background())); err == nil {
		t.Fatal("a long-poll request without a deadline was sent")
	}
}
