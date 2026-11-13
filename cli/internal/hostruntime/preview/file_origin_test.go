package preview

import (
	"bufio"
	"context"
	"errors"
	"io"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/transfername"
)

func sharedFileRoot(t *testing.T, files map[string]string) func() (string, error) {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return func() (string, error) { return root, nil }
}

// fileRequest sends one raw HTTP request to a fresh file origin, as the carrier's raw path does.
func fileRequest(t *testing.T, root func() (string, error), id, raw string) *http.Response {
	t.Helper()
	connection, err := dialFileOrigin(root, id)
	if err != nil {
		t.Fatalf("dialFileOrigin: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	go func() { _, _ = io.WriteString(connection, raw) }()
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestSharedFileContentTypes(t *testing.T) {
	root := sharedFileRoot(t, map[string]string{
		"page.html": "<script>alert(1)</script>", "notes.md": "# hi", "doc.pdf": "%PDF-1.4", "pic.png": "png",
		"draw.svg": "<svg/>", "tool.exe": "MZ", "noext": "data",
	})
	cases := []struct {
		name, contentType, disposition string
		sandbox                        bool
	}{
		{"page.html", "text/html; charset=utf-8", "inline", true},
		{"notes.md", "text/plain; charset=utf-8", "inline", false},
		{"doc.pdf", "application/pdf", "inline", false},
		{"pic.png", "image/png", "inline", false},
		{"draw.svg", "image/svg+xml", "inline", true},
		{"tool.exe", "application/octet-stream", "attachment", false},
		{"noext", "application/octet-stream", "attachment", false},
		{"trail.", "application/octet-stream", "attachment", false}, // saved as "trail_" on every OS
	}
	for _, tc := range cases {
		response := fileRequest(t, root, inbox.FileID(tc.name), "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != tc.contentType {
			t.Fatalf("%s: status %d type %q", tc.name, response.StatusCode, response.Header.Get("Content-Type"))
		}
		if got := response.Header.Get("Content-Disposition"); got != tc.disposition+"; filename*=UTF-8''"+transfername.Portable(tc.name) {
			t.Fatalf("%s: disposition %q", tc.name, got)
		}
		if response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: nosniff missing", tc.name)
		}
		csp := response.Header.Get("Content-Security-Policy")
		if tc.sandbox != strings.HasPrefix(csp, "sandbox;") || tc.sandbox && !strings.Contains(csp, "default-src 'none'") {
			t.Fatalf("%s: CSP %q", tc.name, csp)
		}
	}
}

func TestSharedFileRangeAndLength(t *testing.T) {
	root := sharedFileRoot(t, map[string]string{"data.bin": "0123456789"})
	id := inbox.FileID("data.bin")
	full := fileRequest(t, root, id, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	body, _ := io.ReadAll(full.Body)
	if full.ContentLength != 10 || string(body) != "0123456789" || full.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("full: length %d body %q ranges %q", full.ContentLength, body, full.Header.Get("Accept-Ranges"))
	}
	part := fileRequest(t, root, id, "GET / HTTP/1.1\r\nHost: x\r\nRange: bytes=2-5\r\n\r\n")
	body, _ = io.ReadAll(part.Body)
	if part.StatusCode != http.StatusPartialContent || part.ContentLength != 4 || string(body) != "2345" || part.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range: status %d length %d body %q range %q", part.StatusCode, part.ContentLength, body, part.Header.Get("Content-Range"))
	}
	head := fileRequest(t, root, id, "HEAD / HTTP/1.1\r\nHost: x\r\n\r\n")
	if head.StatusCode != http.StatusOK || head.Header.Get("Content-Length") != "10" {
		t.Fatalf("head: status %d length %q", head.StatusCode, head.Header.Get("Content-Length"))
	}
}

func TestSharedFileOnlyAtRootAndOnlyRead(t *testing.T) {
	root := sharedFileRoot(t, map[string]string{"a.txt": "a"})
	id := inbox.FileID("a.txt")
	if response := fileRequest(t, root, id, "GET /favicon.ico HTTP/1.1\r\nHost: x\r\n\r\n"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("other path: %d", response.StatusCode)
	}
	if response := fileRequest(t, root, id, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n"); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", response.StatusCode)
	}
}

func TestSharedFileGoneOrUnknown(t *testing.T) {
	files := sharedFileRoot(t, map[string]string{"a.txt": "a"})
	if response := fileRequest(t, files, inbox.FileID("other.txt"), "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: %d", response.StatusCode)
	}
	if _, err := dialFileOrigin(nil, inbox.FileID("a.txt")); !errors.Is(err, ErrSharedFileUnavailable) {
		t.Fatalf("no inbox: %v", err)
	}
	root, _ := files()
	connection, err := dialFileOrigin(files, inbox.FileID("a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	// Removed from the Inbox after the dial: the visit gets a 404, not stale bytes.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	go func() { _, _ = io.WriteString(connection, "GET / HTTP/1.1\r\nHost: x\r\n\r\n") }()
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed file: %v %v", response, err)
	}
}

// The browser and inspected paths reach the file through OriginHTTPTransport with the carrier's dialer.
func TestSharedFileThroughOriginTransport(t *testing.T) {
	carrier := &DataCarrierPreviewCarrier{inboxPath: sharedFileRoot(t, map[string]string{"r.txt": "hello"})}
	target := LeaseTarget{Scheme: "file", Address: inbox.FileID("r.txt")}
	transport := carrier.previewHTTPTransport(target)
	defer transport.CloseIdleConnections()
	route := carrier.previewHTTPRoute("route_1", target, "not_applicable", time.Second)
	request, _ := http.NewRequest(http.MethodGet, "https://share.example.test/", nil)
	response, err := transport.RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("status %d body %q", response.StatusCode, body)
	}
	if _, err := (&tunnelmanager.OriginHTTPTransport{}).RoundTrip(context.Background(), hoststate.TunnelConfigRoute{ID: "r", Protocol: "http", OriginScheme: "file", OriginAddress: target.Address, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 1000, DesiredState: "active"}, request); err == nil {
		t.Fatal("a file origin without the carrier's dialer must not dial anything")
	}
}

// A file gone before the visit must reach the browser as 404, not as a transport failure (edge 502).
func TestSharedFileMissingThroughOriginTransportIs404(t *testing.T) {
	carrier := &DataCarrierPreviewCarrier{inboxPath: sharedFileRoot(t, nil)}
	target := LeaseTarget{Scheme: "file", Address: inbox.FileID("gone.txt")}
	transport := carrier.previewHTTPTransport(target)
	defer transport.CloseIdleConnections()
	route := carrier.previewHTTPRoute("route_1", target, "not_applicable", time.Second)
	request, _ := http.NewRequest(http.MethodGet, "https://share.example.test/", nil)
	response, err := transport.RoundTrip(context.Background(), route, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", response.StatusCode)
	}
}

// pb preview --host-header: the origin sees the target's Host instead of the public one; without it the public
// Host passes through. Both the browser and public paths build their route here.
func TestPreviewHTTPRouteCarriesTheTargetHostHeader(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, request.Host)
	}))
	defer origin.Close()
	carrier := &DataCarrierPreviewCarrier{}
	for _, test := range []struct{ name, hostHeader, want string }{
		{"public host kept", "", "share.example.test"},
		{"host header sent", "localhost", "localhost"},
	} {
		target := LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String(), HostHeader: test.hostHeader}
		transport := carrier.previewHTTPTransport(target)
		route := carrier.previewHTTPRoute("route_1", target, "not_applicable", time.Second)
		request, _ := http.NewRequest(http.MethodGet, "https://share.example.test/", nil)
		request.Header.Set("X-Forwarded-Host", "share.example.test")
		response, err := transport.RoundTrip(context.Background(), route, request)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		if string(body) != test.want {
			t.Fatalf("%s: origin saw Host %q, want %q", test.name, body, test.want)
		}
	}
}

func TestLeaseTargetHostHeaderValidation(t *testing.T) {
	for _, test := range []struct {
		target LeaseTarget
		ok     bool
	}{
		{LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "app.example.test"}, true},
		{LeaseTarget{Scheme: "h2c", Address: "127.0.0.1:3000", HostHeader: "localhost"}, true},
		{LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, true},
		{LeaseTarget{Scheme: "unix", Address: "/tmp/app.sock", HostHeader: "app.example.test"}, false},
		{LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "app.example.test:8080"}, false},
		{LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "http://app"}, false},
	} {
		if err := validateLeaseTarget(test.target); (err == nil) != test.ok {
			t.Errorf("%+v: err = %v, want ok = %v", test.target, err, test.ok)
		}
	}
}
