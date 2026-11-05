package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuardPACHandsTheProxyPACToTheGuardAndWithdrawsIt(t *testing.T) {
	const pac = "function FindProxyForURL(url, host) { return 'PROXY 127.0.0.1:1'; }\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(pac)) }))
	defer server.Close()
	var sent []string
	g := guardPAC{set: func(_ context.Context, body string) error { sent = append(sent, body); return nil }, fetch: fetchLoopbackPAC}
	if err := g.Install(t.Context(), server.URL+"/proxy-1.pac"); err != nil {
		t.Fatal(err)
	}
	if err := g.Refresh(t.Context(), server.URL+"/proxy-2.pac"); err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[0] != pac || sent[1] != pac || sent[2] != "" {
		t.Fatalf("guard received %q", sent)
	}
}

func TestGuardPACRefusesAMissingPAC(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	g := guardPAC{set: func(context.Context, string) error { t.Fatal("must not register an unreadable PAC"); return nil }, fetch: fetchLoopbackPAC}
	if err := g.Install(t.Context(), server.URL+"/proxy.pac"); err == nil {
		t.Fatal("a 404 PAC was accepted")
	}
}
