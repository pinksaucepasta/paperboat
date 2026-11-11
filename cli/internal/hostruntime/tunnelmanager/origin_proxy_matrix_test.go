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
	"time"

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
			emit(name, mode.name, browse(t, route, host, "/"))
			if mode.name == "c-https-sni" { // the classes run through the mode that works everywhere today
				for class, probe := range map[string]func(hoststate.TunnelConfigRoute, string) map[string]any{"C8-sse": probeSSE, "C12-range": probeRange, "C13-expect": probeExpect, "F-force-ssl": probeForceSSL} {
					emit(name, class, probe(route, host))
				}
			}
		}
	}
}

func emit(server, mode string, cell map[string]any) {
	cell["server"], cell["mode"] = server, mode
	line, _ := json.Marshal(cell)
	fmt.Println("MATRIX " + string(line))
}

func request(route hoststate.TunnelConfigRoute, host, method, path string, body io.Reader, header map[string]string) (*http.Response, error) {
	req, _ := http.NewRequest(method, "https://"+host+path, body)
	req.Host = host
	req.Header.Set("X-Forwarded-Proto", "https")
	for name, value := range header {
		req.Header.Set(name, value)
	}
	return (&OriginHTTPTransport{}).RoundTrip(context.Background(), route, req)
}

// probeSSE: the first event must reach the visitor before the app has finished (events are 0.7 s apart).
func probeSSE(route hoststate.TunnelConfigRoute, host string) map[string]any {
	start := time.Now()
	response, err := request(route, host, http.MethodGet, "/sse", nil, map[string]string{"Accept": "text/event-stream"})
	if err != nil {
		return map[string]any{"result": "origin_error", "detail": err.Error()}
	}
	defer response.Body.Close()
	first := make([]byte, 1)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		return map[string]any{"result": "origin_error", "detail": err.Error()}
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		return map[string]any{"result": "buffered", "detail": fmt.Sprintf("first event after %dms", elapsed.Milliseconds())}
	}
	return map[string]any{"result": "ok", "detail": fmt.Sprintf("first event after %dms", elapsed.Milliseconds())}
}

func probeRange(route hoststate.TunnelConfigRoute, host string) map[string]any {
	response, err := request(route, host, http.MethodGet, "/range", nil, map[string]string{"Range": "bytes=100-199"})
	if err != nil {
		return map[string]any{"result": "origin_error", "detail": err.Error()}
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusPartialContent || len(body) != 100 {
		return map[string]any{"result": "range_lost", "detail": fmt.Sprintf("status %d, %d bytes", response.StatusCode, len(body))}
	}
	return map[string]any{"result": "ok"}
}

func probeExpect(route hoststate.TunnelConfigRoute, host string) map[string]any {
	response, err := request(route, host, http.MethodPost, "/upload", strings.NewReader(strings.Repeat("x", 5<<20)), map[string]string{"Expect": "100-continue"})
	if err != nil {
		return map[string]any{"result": "origin_error", "detail": err.Error()}
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return map[string]any{"result": "app_refuses", "detail": fmt.Sprintf("status %d", response.StatusCode)}
	}
	return map[string]any{"result": "ok"}
}

// probeForceSSL: an app that redirects to https unless the proxy told it the request was https.
func probeForceSSL(route hoststate.TunnelConfigRoute, host string) map[string]any {
	return browse(nil, route, host, "/force-ssl")
}

// browse follows redirects like a browser does: every hop goes back through the same route,
// because the public hostname always lands on it.
func browse(_ *testing.T, route hoststate.TunnelConfigRoute, host, path string) map[string]any {
	transport := &OriginHTTPTransport{}
	defer transport.CloseIdleConnections()
	next := "https://" + host + path
	for hop := 0; hop < 8; hop++ {
		request, _ := http.NewRequest(http.MethodGet, next, nil)
		request.Host = host
		for name, value := range map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": host, "X-Forwarded-For": "198.51.100.7", "Forwarded": "for=198.51.100.7;proto=https;host=" + host} {
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
