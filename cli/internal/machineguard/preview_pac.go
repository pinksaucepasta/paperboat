package machineguard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Private-preview PAC (macOS system proxy). The guard, not a user's daemon, owns the system-wide auto-proxy
// setting: it needs administrator rights and is one setting for every OS user. The guard serves one PAC URL on
// loopback; each request is attributed to the connecting socket's uid and answered with the PAC that uid's own
// daemon registered ("preview_pac"), so a user's browser is pointed only at that user's preview proxy.

// PreviewPACPort is the loopback port of the guard's PAC endpoint.
const PreviewPACPort = 17391

const (
	previewPACPath    = "/proxy.pac"
	maxPreviewPACBody = 32 << 10
	// directPAC is served to a caller with no registered PAC (no daemon, nothing published): it proxies nothing.
	directPAC = "function FindProxyForURL(url, host) {\n  return 'DIRECT';\n}\n"
)

// PreviewPACURL is the one URL the system auto-proxy setting points at.
func PreviewPACURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(PreviewPACPort) + previewPACPath
}

// setPreviewPAC records conn's PAC; an empty body withdraws it. Called under g.mu.
func (g *guardServer) setPreviewPAC(conn controlConn, body string) error {
	if body == "" {
		delete(g.previewPAC, conn)
		return nil
	}
	if len(body) > maxPreviewPACBody || strings.ContainsRune(body, 0) || !strings.Contains(body, "FindProxyForURL") {
		return errors.New("invalid private preview PAC")
	}
	if g.previewPAC == nil {
		g.previewPAC = map[controlConn]string{}
	}
	g.previewPAC[conn] = body
	return nil
}

// previewPACFor is the PAC of uid's daemon connection, or directPAC.
func (g *guardServer) previewPACFor(uid uint32) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	want := strconv.FormatUint(uint64(uid), 10)
	for conn, body := range g.previewPAC {
		if g.connections[conn] == want {
			return body
		}
	}
	return directPAC
}

// previewPACHandler answers GET /proxy.pac for the uid owner names; an unattributed caller gets directPAC.
func (g *guardServer) previewPACHandler(owner func(*http.Request) (uint32, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != previewPACPath {
			http.NotFound(w, r)
			return
		}
		body := directPAC
		if uid, err := owner(r); err == nil {
			body = g.previewPACFor(uid)
		}
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(body))
	})
}
