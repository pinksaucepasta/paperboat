package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
)

func TestPublicIngressRoutesBareNameAndIPUpstreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	routes := []config.PublicRoute{
		{Host: "name.example.test", Upstream: net.JoinHostPort("localhost", port)},
		{Host: "ip.example.test", PathPrefix: "/downloads", StripPrefix: true, Upstream: net.JoinHostPort(host, port)},
	}
	handler := publicIngressHandler(routes, "infra.example.test", "127.0.0.1:1", http.NotFoundHandler())
	for _, tc := range []struct {
		host, path, want string
	}{
		{host: "name.example.test", path: "/status", want: "/status"},
		{host: "ip.example.test", path: "/downloads/archive.zip", want: "/archive.zip"},
		{host: "ip.example.test", path: "/downloads", want: "/"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+tc.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		body, _ := io.ReadAll(response.Result().Body)
		if response.Code != http.StatusOK || string(body) != tc.want {
			t.Fatalf("%s%s: status=%d body=%q", tc.host, tc.path, response.Code, body)
		}
	}
}

func TestPublicIngressForwardsOnlyBrowserTerminalOnInfrastructureHost(t *testing.T) {
	handler := publicIngressHandler(nil, "infra.example.test", "127.0.0.1:1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/v1/browser-terminal/machine.runtime.example.test", want: http.StatusAccepted},
		{path: "/v1/browser-terminal/", want: http.StatusAccepted},
		{path: "/v1/runtime/machine.runtime.example.test", want: http.StatusAccepted},
		{path: "/v1/other", want: http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://infra.example.test"+tc.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.path, response.Code, tc.want)
		}
	}
}
