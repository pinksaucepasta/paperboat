//go:build darwin || linux || windows

package runtime

import (
	"bufio"
	"context"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNativeInspectorDispatchBoundedGrantAndMethod(t *testing.T) {
	for _, tc := range []struct {
		name, token, method string
		want                int
	}{{"inspect", "grant_test", "GET", 200}, {"wrong grant", "other", "GET", 403}, {"action escalation", "grant_test", "DELETE", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			service := &productionNativePeerService{config: productionNativePeerConfig{inspector: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write([]byte("sanitized"))
			})}}
			header, err := streamauth.NewNativePrivate("credential_test", "inspector", "stream_test", "grant_test", time.Now().Add(time.Minute), 4096, []byte(`{"resource_kind":"preview","resource_id":"preview_test","route_id":"preview_test","action":"inspect"}`))
			if err != nil {
				t.Fatal(err)
			}
			client, host := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan error, 1)
			go func() { defer host.Close(); done <- service.serveInspector(context.Background(), header, host) }()
			request, _ := http.NewRequest(tc.method, "http://inspector.paperboat/v1/inspector/records", nil)
			request.Header.Set("X-Paperboat-Inspector-Grant", tc.token)
			if err = request.Write(client); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(client), request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("status=%d", response.StatusCode)
			}
			client.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("inspector stream leaked")
			}
			if called != (tc.want == 200) {
				t.Fatal("denied request reached inspector handler")
			}
		})
	}
}
