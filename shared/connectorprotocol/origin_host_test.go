package connectorprotocol

import "testing"

func TestOriginHostClasses(t *testing.T) {
	for address, want := range map[string][2]bool{
		"localhost:443":           {true, true},
		"LOCALHOST:443":           {true, true},
		"127.0.0.1:8443":          {true, true},
		"127.9.9.9:443":           {true, true},
		"[::1]:443":               {true, true},
		"10.0.0.5:443":            {false, true},
		"[2001:db8::1]:443":       {false, true},
		"app.internal:443":        {false, false},
		"localhost.evil.test:443": {false, false},
		"not-an-address":          {false, false},
	} {
		if got := OriginHostIsLoopback(address); got != want[0] {
			t.Errorf("OriginHostIsLoopback(%q) = %v", address, got)
		}
		if got := OriginHostIsLocalhostOrIP(address); got != want[1] {
			t.Errorf("OriginHostIsLocalhostOrIP(%q) = %v", address, got)
		}
	}
}
