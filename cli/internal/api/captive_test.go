package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCaptivePortalNamesOnlyAnswersThatAreNotTheServers(t *testing.T) {
	healthy := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":{"status":"healthy"}}`)) }
	cases := []struct {
		name    string
		handler http.HandlerFunc
		portal  bool
	}{
		{"server answers", healthy, false},
		{"redirect to a sign-in host", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://login.example.test/start", http.StatusFound)
		}, true},
		{"sign-in page in place of the answer", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>Sign in</html>")) }, true},
		{"511", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNetworkAuthenticationRequired) }, true},
		{"server outage", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, false},
		{"not found", http.NotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			if got := CaptivePortal(t.Context(), server.URL, nil); got != tc.portal {
				t.Fatalf("CaptivePortal = %v, want %v", got, tc.portal)
			}
		})
	}
	if CaptivePortal(t.Context(), "http://127.0.0.1:1", nil) {
		t.Fatal("an unreachable server was called a captive portal")
	}
	// http to https on the server's own host is not a portal.
	var own *httptest.Server
	own = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+own.Listener.Addr().String()+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer own.Close()
	if CaptivePortal(t.Context(), own.URL, nil) {
		t.Fatal("a redirect to the server's own host was called a captive portal")
	}
}
