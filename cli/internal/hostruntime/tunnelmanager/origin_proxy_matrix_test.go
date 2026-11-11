package tunnelmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

// TestOriginProxyMatrix drives the real origin transport against reverse proxies that force HTTPS
// (Caddy, Traefik, nginx, Apache). tools/device-smoke/origin_proxy_matrix.sh starts the proxies in
// Docker and runs this test with PB_PROXY_MATRIX set to
// "server=httpPort,httpsPort;..." and PB_PROXY_HOST to the public hostname. It prints one JSON line
// per (server, mode) cell and fails when a cell the product promises to serve does not.
func TestOriginProxyMatrix(t *testing.T) {
	spec, host := os.Getenv("PB_PROXY_MATRIX"), os.Getenv("PB_PROXY_HOST")
	if spec == "" || host == "" {
		t.Skip("run tools/device-smoke/origin_proxy_matrix.sh")
	}
	modes := []struct {
		name   string
		scheme string
		sni    *string
	}{{"a-http", "http", nil}, {"b-https", "https", nil}, {"c-https-sni", "https", &host}}
	for _, entry := range strings.Split(spec, ";") {
		name, ports, _ := strings.Cut(entry, "=")
		httpPort, httpsPort, _ := strings.Cut(ports, ",")
		for _, mode := range modes {
			port := httpPort
			if mode.scheme == "https" {
				port = httpsPort
			}
			route := hoststate.TunnelConfigRoute{ID: "route_matrix", Protocol: "http", MatchType: "exact", MatchHostname: host, OriginScheme: mode.scheme, OriginAddress: "localhost:" + port, PreserveHost: true, TLSVerification: "not_applicable", TLSServerName: mode.sni, ConnectTimeoutMs: 3000, IdleTimeoutMs: 5000, DesiredState: "active"}
			if mode.scheme == "https" {
				route.TLSVerification = "system"
			}
			cell := browse(t, route, host)
			cell["server"], cell["mode"] = name, mode.name
			line, _ := json.Marshal(cell)
			fmt.Println("MATRIX " + string(line))
		}
	}
}

// browse follows redirects like a browser does: every hop goes back through the same route,
// because the public hostname always lands on it.
func browse(t *testing.T, route hoststate.TunnelConfigRoute, host string) map[string]any {
	transport := &OriginHTTPTransport{}
	defer transport.CloseIdleConnections()
	next := "https://" + host + "/"
	for hop := 0; hop < 8; hop++ {
		request, _ := http.NewRequest(http.MethodGet, next, nil)
		request.Host = host
		for name, value := range map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": host} {
			request.Header.Set(name, value) // what the edge sets; the origin transport decides what crosses
		}
		response, err := transport.RoundTrip(context.Background(), route, request)
		if err != nil {
			return map[string]any{"result": "origin_error", "hops": hop, "detail": err.Error()}
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			target, err := url.Parse(response.Header.Get("Location"))
			if err != nil || (target.Host != "" && target.Hostname() != host) {
				return map[string]any{"result": "redirect_elsewhere", "hops": hop + 1, "detail": response.Header.Get("Location")}
			}
			next = "https://" + host + target.RequestURI()
			continue
		}
		seen := map[string]any{}
		_ = json.Unmarshal(body, &seen)
		seen["status"] = response.StatusCode
		seen["hops"] = hop
		seen["result"] = "app_refuses"
		if response.StatusCode == 200 {
			seen["result"] = "ok"
		}
		return seen
	}
	return map[string]any{"result": "loop", "hops": 8, "detail": "still redirecting after 8 hops"}
}
