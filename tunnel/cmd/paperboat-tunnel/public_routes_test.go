package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
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
	handler := publicIngressHandler(routes, "infra.example.test", "127.0.0.1:1", http.NotFoundHandler(), nil)
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
	}), nil)
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/v1/browser-terminal/machine.runtime.example.test", want: http.StatusAccepted},
		{path: "/v1/browser-config-compare/machine.runtime.example.test", want: http.StatusAccepted},
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

func TestPublicIngressFailureIsSafeAndRecovers(t *testing.T) {
	if os.Getenv("PB_TUNNEL_PROXY_HELPER") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestPublicIngressFailureIsSafeAndRecovers$")
		command.Env = append(os.Environ(), "PB_TUNNEL_PROXY_HELPER=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated production proxy fixture: %v\n%s", err, output)
		}
		return
	}
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")
	var failing atomic.Bool
	failing.Store(true)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			panic("PRIVATE_ORIGIN_DATA")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.Config.ErrorLog = log.New(io.Discard, "", 0)
	upstream.Start()
	defer upstream.Close()
	reporter, err := reporting.New("paperboat-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous }()
	ctx := reporting.WithSupportReference(t.Context(), "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83")
	handler := publicIngressHandler([]config.PublicRoute{{Host: "public.example.test", Upstream: strings.TrimPrefix(upstream.URL, "http://")}}, "infra.example.test", "127.0.0.1:1", http.NotFoundHandler(), reporter)
	request := func() int {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://public.example.test/PRIVATE_REQUEST_PATH", nil).WithContext(ctx))
		return response.Code
	}
	if status := request(); status != http.StatusBadGateway {
		t.Fatalf("failure status=%d", status)
	}
	failing.Store(false)
	if status := request(); status != http.StatusNoContent {
		t.Fatalf("recovery status=%d", status)
	}
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	var fault reporting.Fault
	if json.Unmarshal(output, &fault) != nil || fault.Code != "upstream_request_failed" || fault.Stage != "forward" || fault.SupportReference != reporting.SupportReference(ctx) || fault.SourceFile != "public_routes.go" {
		t.Fatalf("safe fault metadata missing: %s", output)
	}
	if strings.Contains(string(output), "PRIVATE") || strings.Contains(string(output), upstream.URL) {
		t.Fatal("proxy failure exported private request/origin data")
	}
}
