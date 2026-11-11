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
	const want = `{"publication":{"bytes_per_second":25165824,"byte_burst":1048576},"account":{"bytes_per_second":33554432,"byte_burst":1048576}}`
	if string(raw) != want {
		t.Fatalf("wire = %s", raw)
	}
	for name, bad := range map[string]func(*IngressLimits){
		"zero byte rate":          func(l *IngressLimits) { l.Publication.BytesPerSecond = 0 },
		"tiny byte burst":         func(l *IngressLimits) { l.Account.ByteBurst = 1024 },
		"account below its route": func(l *IngressLimits) { l.Account.BytesPerSecond = l.Publication.BytesPerSecond - 1 },
	} {
		l := Default()
		bad(&l)
		if l.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
