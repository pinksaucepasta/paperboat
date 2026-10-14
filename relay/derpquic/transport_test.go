package derpquic

import "testing"

type observedCarrier struct {
	fakeCarrier
	transport string
}

func (c *observedCarrier) Transport() string { return c.transport }

func TestFallbackCarrierTransportReportsActiveLeg(t *testing.T) {
	preferred := &observedCarrier{transport: "derp_quic"}
	fallback := &observedCarrier{transport: "derp_wss"}
	carrier := NewFallbackCarrier(preferred, fallback, 0)
	if got := carrier.Transport(); got != "" {
		t.Fatalf("unconnected transport=%q", got)
	}
	carrier.mu.Lock()
	carrier.active = fallback
	carrier.mu.Unlock()
	if got := carrier.Transport(); got != "derp_wss" {
		t.Fatalf("fallback transport=%q", got)
	}
	carrier.mu.Lock()
	carrier.active = preferred
	carrier.mu.Unlock()
	if got := carrier.Transport(); got != "derp_quic" {
		t.Fatalf("preferred transport=%q", got)
	}
	if err := carrier.Close(); err != nil {
		t.Fatal(err)
	}
	if got := carrier.Transport(); got != "" {
		t.Fatalf("closed transport=%q", got)
	}
}
