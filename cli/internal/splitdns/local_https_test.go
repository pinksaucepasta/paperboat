package splitdns

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestLocalMachineIndexDoesNotForward(t *testing.T) {
	var dials int
	proxy, err := NewProxy(ProxyConfig{Routes: map[string]BrowserRoute{
		"3000.hp.local.pprbt.dev": {Address: netip.MustParseAddr("127.100.0.4"), Port: 3000, MachineID: "hp"},
		"5173.hp.local.pprbt.dev": {Address: netip.MustParseAddr("127.100.0.4"), Port: 5173, MachineID: "hp"},
	}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials++
		t.Fatal("machine index forwarded")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		host, path, method string
		status             int
	}{
		{"hp.local.pprbt.dev", "/", "GET", 200},
		{"hp.local.pprbt.dev", "/", "HEAD", 200},
		{"hp.local.pprbt.dev", "/", "POST", 405},
		{"hp.local.pprbt.dev", "/secret", "GET", 404},
		{"absent.local.pprbt.dev", "/", "GET", 421},
	} {
		request := httptest.NewRequest(test.method, "https://"+test.host+test.path, nil)
		request.TLS = &tls.ConnectionState{ServerName: test.host}
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("%+v: %d", test, recorder.Code)
		}
		if test.status == 200 && test.method == "GET" && (!strings.Contains(recorder.Body.String(), "https://3000.hp.local.pprbt.dev/") || !strings.Contains(recorder.Body.String(), "https://5173.hp.local.pprbt.dev/")) {
			t.Fatal("index omitted current services")
		}
		if test.method == "HEAD" && recorder.Body.Len() != 0 {
			t.Fatal("HEAD included content")
		}
	}
	request := httptest.NewRequest("GET", "https://hp.local.pprbt.dev/", nil)
	request.TLS = &tls.ConnectionState{ServerName: "3000.hp.local.pprbt.dev"}
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != 421 || dials != 0 {
		t.Fatal("mismatched authority accepted")
	}
}

func TestLocalCRLIsValidatedMetadataOnly(t *testing.T) {
	ca, err := LoadOrCreateConstrainedCA(t.TempDir(), BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	crl, err := ca.RevocationList(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	proxy, err := NewProxy(ProxyConfig{DialContext: func(context.Context, string, string) (net.Conn, error) { t.Fatal("CRL forwarded"); return nil, nil }, RevocationList: func(context.Context, string) ([]byte, []byte, error) { calls++; return ca.caCert.Raw, crl, nil }})
	if err != nil {
		t.Fatal(err)
	}
	path := CRLPath(ca.caCert.Raw)
	for _, test := range []struct {
		method, url string
		status      int
	}{
		{"GET", "http://127.100.0.1" + path, 200},
		{"HEAD", "http://127.100.0.1" + path, 200},
		{"POST", "http://127.100.0.1" + path, 405},
		{"GET", "http://127.100.0.1" + path + "?other=1", 404},
		{"GET", "http://unknown.local.pprbt.dev" + path, 404},
		{"GET", "https://127.100.0.1" + path, 404},
	} {
		request := httptest.NewRequest(test.method, test.url, nil)
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("%+v: %d", test, recorder.Code)
		}
		if test.method == "HEAD" && recorder.Body.Len() != 0 {
			t.Fatal("CRL HEAD included content")
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected metadata calls %d", calls)
	}
	crl[len(crl)-1] ^= 1
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest("GET", "http://127.100.0.1"+path, nil))
	if recorder.Code != 503 {
		t.Fatal("invalid signature served")
	}
}
