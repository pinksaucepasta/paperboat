package runtimeattachment

import "testing"

// The inbox listing of the console reached every machine as "400 invalid_request" because the
// forwarder dropped the X-Paperboat-Operation-Id header the machine's handler requires.
func TestRuntimeOriginForwarderKeepsControlPlaneHeaders(t *testing.T) {
	transport := runtimeOriginForwarder().Transport
	if transport == nil || !transport.KeepControlPlaneHeaders {
		t.Fatalf("the runtime listener's forwarder strips control-plane headers: %+v", transport)
	}
}
