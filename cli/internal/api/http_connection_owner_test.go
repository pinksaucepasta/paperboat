package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestDefaultAPIClientsReuseConnectionWithoutSharingCredentials(t *testing.T) {
	var connections, requests atomic.Int32
	stalled := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		want := fmt.Sprintf("Bearer fixture-client-%d", number)
		if r.URL.Path != "/v1/me" || r.Header.Get("Authorization") != want {
			t.Error("shared connection changed the exact request resource or credential")
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		body := fmt.Sprintf(`{"data":{"id":"account-%d"}}`, number)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = fmt.Fprint(w, body)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		// New network connections can stall while an established one is healthy.
		if state == http.StateNew && connections.Add(1) > 1 {
			<-stalled
		}
	}
	server.Start()
	defer func() { close(stalled); server.Close() }()
	for number := 1; number <= 2; number++ {
		client := New(server.URL, config.Credential{AccessToken: fmt.Sprintf("fixture-client-%d", number)}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		result, err := client.Me(ctx)
		cancel()
		if err != nil {
			t.Fatalf("client %d lost the healthy connection between command steps: %v", number, err)
		}
		if result.ID != fmt.Sprintf("account-%d", number) {
			t.Fatal("client identity changed on the reused connection")
		}
	}
}
