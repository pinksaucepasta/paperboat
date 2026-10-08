package control

import (
	"context"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBrowserConfigCompareControlIsSeparateAndRevocable(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/edge/browser-config-compare/control" || r.Header.Get("X-Paperboat-Edge-Node-ID") != "edge_1" {
			t.Errorf("wrong control binding %s", r.URL.Path)
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, err = conn.Read(r.Context())
		if err != nil {
			return
		}
		err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"host-credential","assignment_id":"assignment_1","attachment_id":"attachment_1"}`))
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusPolicyViolation, "revoked")
	}))
	defer server.Close()
	httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	grant, err := (&BrowserConfigCompareClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}).Admit(ctx, "ticket", "https://dashboard.example.test", "machine.runtime.example.test")
	if err != nil || grant.AssignmentID != "assignment_1" || grant.Credential != "host-credential" {
		t.Fatalf("grant=%#v err=%v", grant, err)
	}
	defer grant.Close()
	select {
	case <-grant.Closed:
	case <-ctx.Done():
		t.Fatal("revocation did not close compare admission")
	}
}
