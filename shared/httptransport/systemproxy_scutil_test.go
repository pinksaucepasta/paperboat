package httptransport

import (
	"errors"
	"testing"
	"time"
)

const scutilSample = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.internal.test
    1 : localhost
    2 : <local>
    3 : 10.0.0.0/8
  }
  ExcludeSimpleHostnames : 1
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 8080
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 8443
  HTTPSProxy : proxy.example.test
}
`

func TestParseScutilProxy(t *testing.T) {
	got := parseScutilProxy(scutilSample)
	if got.HTTPProxy != "http://127.0.0.1:8080" || got.HTTPSProxy != "http://proxy.example.test:8443" {
		t.Fatalf("proxies %q %q", got.HTTPProxy, got.HTTPSProxy)
	}
	if got.NoProxy != ".internal.test,localhost,10.0.0.0/8" || !got.ExcludeSimpleHosts || got.PACOnly || got.Generation == 0 {
		t.Fatalf("snapshot %+v", got)
	}
}

func TestParseScutilProxyDisabledAndPAC(t *testing.T) {
	got := parseScutilProxy("<dictionary> {\n  HTTPEnable : 0\n  HTTPPort : 8080\n  HTTPProxy : 127.0.0.1\n  ProxyAutoConfigEnable : 1\n}\n")
	if got.HTTPProxy != "" || !got.PACOnly {
		t.Fatalf("snapshot %+v", got)
	}
	if got := parseScutilProxy(""); got.HTTPProxy != "" || got.HTTPSProxy != "" || got.PACOnly {
		t.Fatalf("empty %+v", got)
	}
	if got := parseScutilProxy(dict("  HTTPEnable : 1\n  HTTPProxy : x\n  HTTPPort : 99999\n")); got.HTTPProxy != "" {
		t.Fatalf("bad port accepted %+v", got)
	}
}

func TestParseScutilProxyLoopbackPACIsIgnored(t *testing.T) {
	own := dict("  ProxyAutoConfigEnable : 1\n  ProxyAutoConfigURLString : http://127.0.0.1:17391/proxy.pac\n")
	if got := parseScutilProxy(own); got.PACOnly {
		t.Fatalf("own loopback PAC treated as unsupported PAC: %+v", got)
	}
	remote := dict("  ProxyAutoConfigEnable : 1\n  ProxyAutoConfigURLString : http://wpad.example.test/proxy.pac\n")
	if got := parseScutilProxy(remote); !got.PACOnly {
		t.Fatalf("remote PAC not reported: %+v", got)
	}
}

func TestParseScutilProxyIgnoresScopedBlock(t *testing.T) {
	out := scutilSample[:len(scutilSample)-2] + `  __SCOPED__ : <dictionary> {
    en0 : <dictionary> {
      ExceptionsList : <array> {
        0 : scoped.test
      }
      HTTPEnable : 1
      HTTPPort : 1
      HTTPProxy : scoped.example.test
    }
  }
}
`
	got := parseScutilProxy(out)
	if got.HTTPProxy != "http://127.0.0.1:8080" || got.NoProxy != ".internal.test,localhost,10.0.0.0/8" {
		t.Fatalf("scoped values leaked: %+v", got)
	}
}

func dict(body string) string { return "<dictionary> {\n" + body + "}\n" }

func TestSnapshotFromScutilFailureIsDirectAndLoggedOnce(t *testing.T) {
	var logged int
	scutilWarn.Lock()
	scutilWarn.at, scutilWarn.log = time.Time{}, func(string, ...any) { logged++ }
	scutilWarn.Unlock()
	for range 3 {
		if got := snapshotFromScutil(nil, errors.New("exec failed")); got != (ProxySnapshot{}) {
			t.Fatalf("failed scutil run = %+v, want no proxy", got)
		}
	}
	if logged != 1 {
		t.Fatalf("logged %d times, want once within the interval", logged)
	}
	if got := snapshotFromScutil([]byte(scutilSample), nil); got.HTTPProxy == "" {
		t.Fatalf("successful run lost its proxy: %+v", got)
	}
	pac := "<dictionary> {\n  ProxyAutoConfigEnable : 1\n  ProxyAutoConfigURLString : http://pac.example.test/a.pac\n}\n"
	if got := snapshotFromScutil([]byte(pac), nil); !got.PACOnly {
		t.Fatalf("unsupported PAC config must stay PACOnly: %+v", got)
	}
}
