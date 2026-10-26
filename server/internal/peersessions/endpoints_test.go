package peersessions

import (
	"errors"
	"fmt"
	"testing"
)

func TestReportedEndpointsAreNormalized(t *testing.T) {
	got, err := normalizeEndpoints([]string{"203.0.113.7:41641", "[2001:db8::1]:41641", "[::ffff:198.51.100.2]:9", "203.0.113.7:41641", "10.0.0.5:41641"})
	if want := "[10.0.0.5:41641 198.51.100.2:9 203.0.113.7:41641 [2001:db8::1]:41641]"; err != nil || fmt.Sprint(got) != want {
		t.Fatalf("normalized %v, %v; want %s", got, err, want)
	}
	if got, err := normalizeEndpoints(nil); got != nil || err != nil {
		t.Fatalf("not reported: %v, %v", got, err)
	}
	if got, err := normalizeEndpoints([]string{}); got == nil || len(got) != 0 || err != nil {
		t.Fatalf("an empty report is an empty set: %v, %v", got, err)
	}
	for _, bad := range [][]string{
		{"127.0.0.1:41641"}, {"0.0.0.0:41641"}, {"[::]:41641"}, {"224.0.0.1:41641"}, {"203.0.113.7:0"}, {"203.0.113.7"},
		{"host.example.test:41641"}, {"[fe80::1%eth0]:41641"},
		{"1.0.0.1:1", "1.0.0.2:1", "1.0.0.3:1", "1.0.0.4:1", "1.0.0.5:1", "1.0.0.6:1", "1.0.0.7:1", "1.0.0.8:1", "1.0.0.9:1"},
	} {
		if _, err := normalizeEndpoints(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v, want ErrInvalid", bad, err)
		}
	}
}
