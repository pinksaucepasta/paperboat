//go:build darwin

package machineservices

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

func TestParseNetstatWebPorts(t *testing.T) {
	data := []byte(`Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  *.443                  *.*                    LISTEN
tcp46      0      0  *.80                   *.*                    LISTEN
tcp4       0      0  127.100.0.1.80         *.*                    LISTEN
tcp6       0      0  ::1.443                *.*                    LISTEN
tcp4       0      0  *.22                   *.*                    LISTEN
tcp4       0      0  192.0.2.5.51000        192.0.2.9.443          ESTABLISHED
`)
	got := parseNetstatWebPorts(data)
	want := []Service{{Port: 443, Loopback: "127.0.0.1"}, {Port: 80, Loopback: "127.0.0.1"}, {Port: 80, Loopback: "::1"}, {Port: 443, Loopback: "::1"}}
	if len(got) != len(want) {
		t.Fatalf("services=%#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("services=%#v", got)
		}
	}
}
