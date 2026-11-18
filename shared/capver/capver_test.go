package capver

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestTransportSendsVersionAndFromRequestReadsIt(t *testing.T) {
	var got int
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = FromRequest(r) }))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := (&http.Client{Transport: Transport(nil)}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got != Version || request.Header.Get(Header) != "" {
		t.Fatalf("server saw %d, caller request header %q", got, request.Header.Get(Header))
	}
	for value, want := range map[string]int{"": 0, "x": 0, "-1": 0, strconv.Itoa(Version + 4): Version + 4} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(Header, value)
		if FromRequest(r) != want {
			t.Errorf("FromRequest(%q) != %d", value, want)
		}
	}
}
