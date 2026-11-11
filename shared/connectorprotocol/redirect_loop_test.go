package connectorprotocol

import (
	"net/http"
	"testing"
)

func TestIsSelfRedirect(t *testing.T) {
	redirect := func(status int, location string, cookie bool) *http.Response {
		response := &http.Response{StatusCode: status, Header: http.Header{"Location": {location}}}
		if cookie {
			response.Header.Set("Set-Cookie", "seen=1")
		}
		return response
	}
	for _, test := range []struct {
		name     string
		method   string
		uri      string
		response *http.Response
		want     bool
	}{
		{"caddy port 80 redirect", "GET", "/a?b=1", redirect(308, "https://app.example.test/a?b=1", false), true},
		{"explicit 443 and host case", "GET", "/", redirect(301, "https://APP.example.test:443/", false), true},
		{"relative self", "HEAD", "/x", redirect(302, "/x", false), true},
		{"empty path is root", "GET", "/", redirect(307, "https://app.example.test", false), true},
		{"other path", "GET", "/", redirect(302, "https://app.example.test/login", false), false},
		{"other host", "GET", "/", redirect(302, "https://www.example.test/", false), false},
		{"plain http target", "GET", "/", redirect(301, "http://app.example.test/", false), false},
		{"other port", "GET", "/", redirect(301, "https://app.example.test:8443/", false), false},
		{"post redirect get", "POST", "/form", redirect(302, "/form", false), false},
		{"sets a cookie first", "GET", "/", redirect(302, "/", true), false},
		{"303 not counted", "GET", "/", redirect(303, "/", false), false},
		{"no location", "GET", "/", redirect(301, "", false), false},
	} {
		if got := IsSelfRedirect(test.method, "app.example.test", test.uri, test.response); got != test.want {
			t.Errorf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}
