package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
)

func TestSharedTLSBrowserAuthorization(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized", true: "revoked"}[denied], func(t *testing.T) {
			match, decision := browserTestMatch()
			decision.ExpiresAt = time.Now().Add(8 * time.Second)
			authority := &browserAuthorityTest{decision: decision}
			if denied {
				authority.failure = control.ErrBrowserDenied
			}
			browser := &BrowserAccess{Authority: authority, LoginOrigin: "https://login.example.test"}
			clientTLS, serverTLS, _, _, _ := task24Certificates(t, match.Host)
			clientTLS.ServerName = match.Host
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			defer registry.Close()
			shared, err := NewSharedTLSListener(raw, &tlsAuthorityFixture{}, registry, func(host string) bool { return host == match.Host }, 4, "")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, finish, ok := browser.authorize(w, r, match)
				defer finish()
				if ok {
					_, _ = io.WriteString(w, "authorized application")
				}
			})}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			transport := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, raw.Addr().String())
			}}
			defer transport.CloseIdleConnections()
			request, _ := http.NewRequest("GET", "https://"+match.Host+"/", nil)
			request.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "token"})
			response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.ProtoMajor != 2 {
				t.Fatal("browser HTTP2 not preserved")
			}
			if denied {
				if response.StatusCode != 404 {
					t.Fatalf("revoked browser status=%d", response.StatusCode)
				}
			} else if response.StatusCode != 200 || string(body) != "authorized application" {
				t.Fatalf("authorized browser status=%d", response.StatusCode)
			}
		})
	}
}

type sharedTLSOutageAuthority struct {
	tlsAuthorityFixture
	unavailable atomic.Bool
}

func (a *sharedTLSOutageAuthority) Snapshot(ctx context.Context) ([]connectorprotocol.IngressDecision, error) {
	if a.unavailable.Load() {
		return nil, errors.New("control unavailable")
	}
	return a.tlsAuthorityFixture.Snapshot(ctx)
}
func TestSharedTLSAuthorityOutageAndRecovery(t *testing.T) {
	for _, host := range []string{"infrastructure.test", "application.test"} {
		t.Run(host, func(t *testing.T) {
			clientTLS, serverTLS, _, _, _ := task24Certificates(t, host)
			clientTLS.ServerName = host
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
			defer registry.Close()
			authority := &sharedTLSOutageAuthority{}
			authority.unavailable.Store(true)
			shared, err := NewSharedTLSListener(raw, authority, registry, func(name string) bool { return name == host }, 4, "infrastructure.test")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{TLSConfig: serverTLS, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ready") })}
			defer server.Close()
			go server.ServeTLS(shared, "", "")
			dial := func() error {
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), clientTLS)
				if err == nil {
					conn.Close()
				}
				return err
			}
			err = dial()
			if host == "infrastructure.test" && err != nil {
				t.Fatal("reserved infrastructure unavailable during control outage", err)
			}
			if host == "application.test" && err == nil {
				t.Fatal("dynamic hostname accepted without authority")
			}
			authority.unavailable.Store(false)
			if err = dial(); err != nil {
				t.Fatal("fresh authority did not restore HTTPS", err)
			}
		})
	}
}
