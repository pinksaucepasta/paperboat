package api

import "testing"

// F20-03: a TLS passthrough route is created like public TCP (the tunnel's managed hostname and a
// tcp origin); `pb tunnel route add --protocol tls` was refused before.
func TestTLSPassthroughRouteInputUsesTheManagedHostnameAndATCPOrigin(t *testing.T) {
	tls := TunnelRouteInput{Name: "tls", Protocol: "tls", HostMatch: TunnelRouteHostMatch{Type: "managed_exact", Hostname: "brave-blue-beacon-0421.tunnel.example.test"}, Origin: TunnelRouteOrigin{Scheme: "tcp", Address: "127.0.0.1:8443"}}
	if err := validateTunnelRouteInput(tls, true); err != nil {
		t.Fatalf("TLS passthrough route refused: %v", err)
	}
	tls.Origin.PreserveHost = true
	if err := validateTunnelRouteInput(tls, true); err == nil {
		t.Fatal("TLS passthrough route preserving an HTTP Host header accepted")
	}
	tls.Origin.PreserveHost = false
	tls.HostMatch = TunnelRouteHostMatch{Type: "catch_all"}
	if err := validateTunnelRouteInput(tls, true); err == nil {
		t.Fatal("TLS passthrough route without the managed hostname accepted")
	}
}
