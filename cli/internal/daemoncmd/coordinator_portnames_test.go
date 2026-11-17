package daemoncmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
	"github.com/pinksaucepasta/paperboat/cli/internal/testcert"
)

// Server port names route on this computer with nothing configured here: <name>.<machine>.<domain> opens the
// named port, an unmatched name goes to the machine's front desk, and a top-level <name>.<domain> opens its port
// until a machine takes that name.
func TestCoordinatorRoutesServerPortNamesFrontDeskAndTopLevelNames(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	// One origin per machine port answers with its port, so a response names the port its request reached
	// (the gateway pools upstream connections, so dials cannot be counted).
	origins := map[int]*httptest.Server{}
	for _, port := range []int{443, 3000} {
		origins[port] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "%s %d", r.Host, port) }))
		defer origins[port].Close()
	}
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(ctx context.Context, id string, port int) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", origins[port].Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) { return ca.TLS(name) }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	c.portNames = map[string]api.MachineServicesMachine{"home": {
		PortNames:     []api.PortName{{Name: "app", Port: 3000}, {Name: "unlisted", Port: 9999}},
		TopLevelNames: []api.PortName{{Name: "jellyfin", Port: 3000}, {Name: "gateway", Port: 3000}},
		FrontDesk:     api.FrontDesk{Port: 443, Scheme: "http"}, // a proxy speaking plain http on 443
	}}
	c.machineAliases = map[string]bool{"homelab": true}
	peer := &daemonrpc.PeerUpdate{PeerId: "home", Alias: "homelab", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{443, 3000}}
	if err := c.replaceGuardedRoutes(t.Context(), []*daemonrpc.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	open := func(host string, port int) {
		t.Helper()
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host}}
		defer tr.CloseIdleConnections()
		req, _ := http.NewRequest("GET", "https://"+names.addresses[443]+"/", nil)
		req.Host = host
		res, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(host, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if want := fmt.Sprintf("%s %d", host, port); res.StatusCode != 200 || string(body) != want {
			t.Fatalf("%s: %d %q, want %q", host, res.StatusCode, body, want)
		}
	}
	for host, port := range map[string]int{
		"app.homelab." + testBrowserDomain:      3000, // a server name
		"coolify.homelab." + testBrowserDomain:  443,  // unmatched: the front desk
		"unlisted.homelab." + testBrowserDomain: 443,  // a name of an unlisted port was never delivered
		"jellyfin." + testBrowserDomain:         3000, // a top-level name
	} {
		open(host, port)
	}
	web := c.webRoutes[splitdns.BrowserGatewayHostname]
	if web.browserRoutes["*.homelab."+testBrowserDomain].HTTPS || !web.browserRoutes["443.homelab."+testBrowserDomain].HTTPS {
		t.Fatal("the front desk's scheme does not decide how unmatched names reach it")
	}
	if _, ok := web.browserRoutes["gateway."+testBrowserDomain]; ok {
		t.Fatal("a top-level name took the gateway label")
	}
	// A machine named jellyfin joins: jellyfin.<domain> is that machine's, not the top-level name's.
	c.machineAliases["jellyfin"] = true
	if err := c.replaceGuardedRoutes(t.Context(), []*daemonrpc.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if _, ok := web.browserRoutes["jellyfin."+testBrowserDomain]; ok {
		t.Fatal("a top-level name shadowed by a machine still routes")
	}
}
