package edgehttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The edge never tells a browser it speaks HTTP/3: a node that does not hold a name must not be moved to UDP by an
// Alt-Svc, because only the TCP path is forwarded to the holder. No response, whether the app answered (its own
// Alt-Svc describes the origin) or the edge did (a name it does not serve), carries one.
func TestNoAltSvcAdvertisedForAnyName(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		_, _ = w.Write([]byte("app"))
	}))
	defer upstream.Close()
	gateway, err := NewGateway(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 1024,
		Readiness: readinessFunc(func(host string) (string, string, bool) {
			return "ready", "", host == "held.preview.example.test"
		})}, strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	for host, wantApp := range map[string]bool{"held.preview.example.test": true, "other.preview.example.test": false, "nobody.example.test": false, "other.tunnels.example.test": false} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host = host
		gateway.ServeHTTP(recorder, request)
		if app := recorder.Body.String() == "app"; app != wantApp {
			t.Fatalf("%s: answered by the app = %v (status %d), want %v", host, app, recorder.Code, wantApp)
		}
		for name, values := range recorder.Header() {
			if strings.EqualFold(name, "Alt-Svc") || strings.Contains(strings.ToLower(strings.Join(values, ",")), "h3") {
				t.Fatalf("%s: response advertises HTTP/3: %s: %v", host, name, values)
			}
		}
	}
}
