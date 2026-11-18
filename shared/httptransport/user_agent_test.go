package httptransport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/capver"
)

// A request with no User-Agent gets the configured one (so the server log counts every machine process under the
// build's agent), an explicit one is left alone, and the caller's request is not modified.
func TestTransportSetsTheConfiguredUserAgentOnlyWhenMissing(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		if capver.FromRequest(r) != capver.Version {
			t.Errorf("capability version = %q", r.Header.Get(capver.Header))
		}
	}))
	defer server.Close()
	config := DevelopmentConfig()
	config.ProxySource = StaticProxySource{}
	config.UserAgent = "paperboat/test-build"
	transport, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}
	plain, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	explicit, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	explicit.Header.Set("User-Agent", "paperboat/explicit")
	for _, request := range []*http.Request{plain, explicit} {
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if len(seen) != 2 || seen[0] != "paperboat/test-build" || seen[1] != "paperboat/explicit" {
		t.Fatalf("user agents = %q", seen)
	}
	if plain.Header.Get("User-Agent") != "" || plain.Header.Get(capver.Header) != "" {
		t.Fatal("the caller's request was modified")
	}
}
