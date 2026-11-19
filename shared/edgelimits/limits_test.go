package edgelimits

import (
	"encoding/json"
	"testing"
)

func TestDefaultLimitsAreValidAndKeepOneStreamAbove150Mbps(t *testing.T) {
	d := Default()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if bits := d.Publication.BytesPerSecond * 8; bits < 150_000_000 {
		t.Fatalf("publication byte rate %d bit/s is below PR-06's 150 Mbit/s", bits)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"publication":{"bytes_per_second":25165824,"byte_burst":1048576},"account":{"bytes_per_second":33554432,"byte_burst":1048576},"free_requests":{"per_address":{"per_second":10,"burst":1500},"per_publication":{"per_second":50,"burst":3000}}}`
	if string(raw) != want {
		t.Fatalf("wire = %s", raw)
	}
	for name, bad := range map[string]func(*IngressLimits){
		"zero byte rate":                 func(l *IngressLimits) { l.Publication.BytesPerSecond = 0 },
		"tiny byte burst":                func(l *IngressLimits) { l.Account.ByteBurst = 1024 },
		"zero request rate":              func(l *IngressLimits) { l.FreeRequests.PerAddress.PerSecond = 0 },
		"zero request burst":             func(l *IngressLimits) { l.FreeRequests.PerPublication.Burst = 0 },
		"pricing link that is not https": func(l *IngressLimits) { l.FreeRequests.PricingURL = "javascript:alert(1)" },
		"account below its route":        func(l *IngressLimits) { l.Account.BytesPerSecond = l.Publication.BytesPerSecond - 1 },
	} {
		l := Default()
		bad(&l)
		if l.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
