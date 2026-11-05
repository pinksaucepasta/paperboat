package machineguard

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type pacConn struct{ controlConn }

const userPAC = "function FindProxyForURL(url, host) {\n  return 'PROXY 127.100.0.9:1';\n}\n"

func TestPreviewPACIsPerUserAndDirectForOthers(t *testing.T) {
	alice, bob := &pacConn{}, &pacConn{}
	g := &guardServer{connections: map[controlConn]string{alice: "501", bob: "502"}}
	if err := g.setPreviewPAC(alice, userPAC); err != nil {
		t.Fatal(err)
	}
	if got := g.previewPACFor(501); got != userPAC {
		t.Fatalf("uid 501 PAC = %q", got)
	}
	if got := g.previewPACFor(502); got != directPAC {
		t.Fatalf("uid 502 without a PAC must get DIRECT, got %q", got)
	}
	if err := g.setPreviewPAC(alice, ""); err != nil || g.previewPACFor(501) != directPAC {
		t.Fatalf("empty body must withdraw the PAC: %v", err)
	}
}

func TestPreviewPACRejectsInvalidBodies(t *testing.T) {
	g := &guardServer{}
	for _, body := range []string{"not a pac", "FindProxyForURL\x00", strings.Repeat("x", maxPreviewPACBody) + "FindProxyForURL"} {
		if err := g.setPreviewPAC(&pacConn{}, body); err == nil {
			t.Fatalf("accepted %d byte body", len(body))
		}
	}
}

func TestPreviewPACHandlerServesAttributedUserOnly(t *testing.T) {
	conn := &pacConn{}
	g := &guardServer{connections: map[controlConn]string{conn: "501"}}
	if err := g.setPreviewPAC(conn, userPAC); err != nil {
		t.Fatal(err)
	}
	get := func(method, path string, uid uint32, ownerErr error) (int, string) {
		h := g.previewPACHandler(func(*http.Request) (uint32, error) { return uid, ownerErr })
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		body, _ := io.ReadAll(rec.Body)
		return rec.Code, string(body)
	}
	if code, body := get("GET", previewPACPath, 501, nil); code != 200 || body != userPAC {
		t.Fatalf("attributed user: %d %q", code, body)
	}
	if code, body := get("GET", previewPACPath, 0, errors.New("unattributed")); code != 200 || body != directPAC {
		t.Fatalf("unattributed caller must get DIRECT: %d %q", code, body)
	}
	if code, _ := get("POST", previewPACPath, 501, nil); code != 404 {
		t.Fatalf("POST: %d", code)
	}
	if got := PreviewPACURL(); got != "http://127.0.0.1:17391/proxy.pac" {
		t.Fatalf("PAC URL %q", got)
	}
}
