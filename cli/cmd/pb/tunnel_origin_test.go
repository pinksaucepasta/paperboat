package main

import "testing"

// pb tunnel create --from tcp://... is the public TCP path; the shared origin parser must accept it.
func TestTunnelOriginAcceptsATCPOrigin(t *testing.T) {
	scheme, address, err := splitTunnelOrigin("tcp://127.0.0.1:5432")
	if err != nil || scheme != "tcp" || address != "127.0.0.1:5432" {
		t.Fatalf("tcp origin = %q %q %v", scheme, address, err)
	}
	if _, _, err := splitTunnelOrigin("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("ftp origin accepted")
	}
}
