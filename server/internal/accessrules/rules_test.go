package accessrules

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeMasksDedupesAndBounds(t *testing.T) {
	got, err := Normalize(Rules{Allow: []string{" 10.1.2.3/8 ", "10.0.0.0/8", "::ffff:192.0.2.1", "2001:db8::5/48"}, Deny: nil})
	if err != nil || strings.Join(got.Allow, ",") != "10.0.0.0/8,192.0.2.1/32,2001:db8::/48" || got.Deny == nil || len(got.Deny) != 0 {
		t.Fatalf("normalized = %+v %v", got, err)
	}
	for name, bad := range map[string]Rules{
		"text": {Allow: []string{"office"}},
		"zone": {Deny: []string{"fe80::1%eth0"}},
		"too many": {Allow: func() []string {
			out := make([]string, MaxEntries+1)
			for i := range out {
				out[i] = fmt.Sprintf("10.0.%d.%d", i/256, i%256)
			}
			return out
		}()},
	} {
		if _, err := Normalize(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
