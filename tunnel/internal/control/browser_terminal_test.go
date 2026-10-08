package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrowserTerminalControlChannelIsNodeBoundAndFailClosed(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/edge/browser-terminal/control" || r.Header.Get("Authorization") != "Bearer "+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || r.Header.Get("X-Paperboat-Edge-Node-ID") != "edge_1" || r.Header.Get("X-Paperboat-Edge-Process-Epoch") != "epoch_01" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		kind, raw, err := connection.Read(r.Context())
		if err != nil || kind != websocket.MessageText || string(raw) != `{"ticket":"one-use","origin":"https://dashboard.example.test","host":"machine.runtime.example.test"}` {
			return
		}
		requests <- struct{}{}
		if connection.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"host-credential","terminal_session_id":"term_1","attachment_id":"attach_1"}`)) != nil {
			return
		}
		// A revoked control channel must end the edge attachment even though
		// the host credential was already issued.
		_ = connection.Close(websocket.StatusPolicyViolation, "revoked")
	}))
	defer server.Close()
	httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: 3 * time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admission, err := client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
	if err != nil || admission.Credential != "host-credential" || admission.TerminalSessionID != "term_1" || admission.AttachmentID != "attach_1" {
		t.Fatalf("admission: %v", err)
	}
	defer admission.Close()
	select {
	case <-requests:
	case <-ctx.Done():
		t.Fatal("control request did not reach server")
	}
	select {
	case <-admission.Closed:
	case <-ctx.Done():
		t.Fatal("revoked control did not close attachment")
	}
}
