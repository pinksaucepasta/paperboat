package native_test

import (
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go4.org/mem"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

// runDERPAndSTUNWithServer is tailscale's integration.RunDERPAndSTUN (same server and map) that also returns the
// DERP server, so a test can wait for an engine's DERP connection instead of sleeping and hoping it is up: a flight
// sent before the peer is connected is dropped and waits for the one-second resend.
func runDERPAndSTUNWithServer(t testing.TB, ip string) (*tailcfg.DERPMap, *derpserver.Server) {
	t.Helper()
	logf := func(string, ...any) {}
	d := derpserver.New(key.NewNode(), logf)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	httpsrv := httptest.NewUnstartedServer(derpserver.AddWebSocketSupport(d, derpserver.Handler(d)))
	httpsrv.Listener.Close()
	httpsrv.Listener = ln
	httpsrv.Config.ErrorLog = logger.StdLogger(logf)
	httpsrv.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	httpsrv.StartTLS()
	stunAddr, stunCleanup := stuntest.ServeWithPacketListener(t, nettype.Std{})
	t.Cleanup(func() {
		httpsrv.CloseClientConnections()
		httpsrv.Close()
		d.Close()
		stunCleanup()
		ln.Close()
	})
	return &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{1: {RegionID: 1, RegionCode: "test", Nodes: []*tailcfg.DERPNode{{
		Name: "t1", RegionID: 1, HostName: ip, IPv4: ip, IPv6: "none", STUNPort: stunAddr.Port,
		DERPPort: httpsrv.Listener.Addr().(*net.TCPAddr).Port, InsecureForTests: true, STUNTestIP: ip,
	}}}}}, d
}

// waitDERPConnected waits until the DERP server sees (connected) or no longer sees (!connected) the node key.
func waitDERPConnected(t testing.TB, d *derpserver.Server, nodeKey string, connected bool) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(nodeKey) // the bindings carry the key as unpadded base64
	if err != nil || len(raw) != 32 {
		t.Fatalf("node key %q is not 32 bytes of unpadded base64url: %v", nodeKey, err)
	}
	public := key.NodePublicFromRaw32(mem.B(raw))
	for deadline := time.Now().Add(15 * time.Second); d.IsClientConnectedForTest(public) != connected; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("DERP connection of the peer did not reach connected=%v", connected)
		}
	}
}
