package edgehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSelfHostedWithoutManagedDomainsFailsClosed(t *testing.T) {
	nextCalls := 0
	policy, err := New(Config{SelfHosted: true, MaxHeaderBytes: 4096, MaxBodyBytes: 1024}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ }))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"preview.example.test", "runtime.example.test", "node.example.test"} {
		r := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		w := httptest.NewRecorder()
		policy.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("unconfigured host status %d", w.Code)
		}
	}
	if nextCalls != 0 {
		t.Fatal("unconfigured host was forwarded")
	}
	if _, err := New(Config{MaxHeaderBytes: 4096, MaxBodyBytes: 1024}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Fatal("managed edge accepted unconfigured domains")
	}
}
