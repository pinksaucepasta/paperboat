package configsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// TestControlClientJudgesExpiryByTheServerClock: on a machine 10 minutes behind the server, the
// production client (no Clock) judges credential and lease expiry by the server's clock (F21-02).
func TestControlClientJudgesExpiryByTheServerClock(t *testing.T) {
	skew := 10 * time.Minute
	serverclock.Observe(time.Now().UTC().Add(skew).Format(http.TimeFormat), time.Now(), time.Now())
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	client, err := NewControlClient(ControlClientConfig{
		BaseURL: server.URL, AllowedHosts: []string{"127.0.0.1"},
		Identities: tokenSourceFunc(func(context.Context) (string, error) { return "identity", nil }),
		Proofs:     &recordingProofSource{}, OperationID: func() (string, error) { return "operation", nil },
		Transport: server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.clock().Sub(time.Now()); (got - skew).Abs() > 5*time.Second {
		t.Fatalf("client clock is %v from local, want the server's %v", got, skew)
	}
}
