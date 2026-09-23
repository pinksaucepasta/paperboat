//go:build darwin

package deviceservices

import "testing"

func TestParseLsofFiltersOwnerAndExactLoopback(t *testing.T) {
	data := []byte("p10\nu501\nn127.0.0.1:5432\nn127.0.0.2:5433\np11\nu502\nn*:5434\np12\nu501\nn*:5435\n")
	got := parseLsof(data, 501, "127.0.0.1")
	if len(got) != 2 || got[0] != (Service{Port: 5432, Loopback: "127.0.0.1"}) || got[1] != (Service{Port: 5435, Loopback: "127.0.0.1"}) {
		t.Fatalf("services=%#v", got)
	}
}

func TestParseLsofIPv6(t *testing.T) {
	got := parseLsof([]byte("p10\nu501\nn[::1]:8080\nn[::2]:8081\n"), 501, "::1")
	if len(got) != 1 || got[0] != (Service{Port: 8080, Loopback: "::1"}) {
		t.Fatalf("services=%#v", got)
	}
}
