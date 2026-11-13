package httptransport

import (
	"encoding/binary"
	"testing"
)

func TestSnapshotFromWinINET(t *testing.T) {
	cases := []struct {
		name                  string
		enabled               bool
		server, override, pac string
		http, https, noProxy  string
		simple, pacOnly       bool
	}{
		{name: "one proxy for every scheme", enabled: true, server: "proxy.example.test:3128", http: "http://proxy.example.test:3128", https: "http://proxy.example.test:3128"},
		{name: "per scheme, https goes direct when absent", enabled: true, server: "http=a.example.test:80;socks=s.example.test:1080", http: "http://a.example.test:80"},
		{name: "per scheme both", enabled: true, server: "http=a.example.test:80;https=https://b.example.test:8443", http: "http://a.example.test:80", https: "http://b.example.test:8443"},
		{name: "override list and local", enabled: true, server: "p.example.test:1", override: "*.corp.test;10.0.0.1;<local>", http: "http://p.example.test:1", https: "http://p.example.test:1", noProxy: ".corp.test,10.0.0.1", simple: true},
		{name: "disabled proxy is direct", enabled: false, server: "p.example.test:1"},
		{name: "invalid port is ignored", enabled: true, server: "p.example.test:x"},
		{name: "remote PAC without proxy refuses", pac: "http://wpad.example.test/proxy.pac", pacOnly: true},
		{name: "loopback PAC is ours", pac: "http://127.0.0.1:17391/proxy.pac"},
		{name: "explicit proxy beats PAC", enabled: true, server: "p.example.test:1", pac: "http://wpad.example.test/proxy.pac", http: "http://p.example.test:1", https: "http://p.example.test:1"},
	}
	for _, c := range cases {
		got := snapshotFromWinINET(c.enabled, c.server, c.override, c.pac)
		if got.HTTPProxy != c.http || got.HTTPSProxy != c.https || got.NoProxy != c.noProxy || got.ExcludeSimpleHosts != c.simple || got.PACOnly != c.pacOnly {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if a, b := snapshotFromWinINET(true, "a.example.test:1", "", ""), snapshotFromWinINET(true, "b.example.test:1", "", ""); a.Generation == b.Generation {
		t.Fatal("a changed proxy keeps the generation")
	}
}

func winHTTPBlob(flags byte, proxy, bypass string) []byte {
	blob := make([]byte, 12)
	blob[0], blob[8] = 0x18, flags
	for _, value := range []string{proxy, bypass} {
		blob = binary.LittleEndian.AppendUint32(blob, uint32(len(value)))
		blob = append(blob, value...)
	}
	return blob
}

func TestParseWinHTTPSettings(t *testing.T) {
	got := parseWinHTTPSettings(winHTTPBlob(0x03, "proxy.example.test:3128", "*.corp.test;<local>"))
	if got.HTTPSProxy != "http://proxy.example.test:3128" || got.NoProxy != ".corp.test" || !got.ExcludeSimpleHosts {
		t.Fatalf("%+v", got)
	}
	for name, blob := range map[string][]byte{
		"direct":    winHTTPBlob(0x01, "proxy.example.test:3128", ""),
		"truncated": winHTTPBlob(0x03, "proxy.example.test:3128", "")[:14],
		"short":     {1, 2, 3},
		"overlong":  append(winHTTPBlob(0x03, "", "")[:12], 0xff, 0xff, 0xff, 0x7f),
	} {
		if got := parseWinHTTPSettings(blob); got.HTTPProxy != "" || got.HTTPSProxy != "" {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
