//go:build linux

package deviceservices

import "testing"

func TestParseProcTCPFiltersOwnerAndExactLoopback(t *testing.T) {
	data := []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1538 00000000:0000 0A 0:0 00:0 0 1000 0 1
   1: 0100007F:1539 00000000:0000 0A 0:0 00:0 0 1000 0 2
   2: 0200007F:153A 00000000:0000 0A 0:0 00:0 0 1000 0 3
   3: 0100007F:153B 00000000:0000 0A 0:0 00:0 0 1001 0 4
   4: 0100007F:153C 00000000:0000 01 0:0 00:0 0 1000 0 5
   5: 0100007F:153D 00000000:0000 0A 0:0 00:0 0 invalid 0 6
`)
	got := parseProcTCP(data, false, 1000)
	if len(got) != 2 || got[0] != (Service{Port: 5432, Loopback: "127.0.0.1"}) || got[1] != (Service{Port: 5433, Loopback: "127.0.0.1"}) {
		t.Fatalf("services=%#v", got)
	}
}

func TestParseProcTCP6FiltersOwner(t *testing.T) {
	data := []byte("0: 00000000000000000000000000000000:1538 00:0000 0A 0:0 00:0 0 42 0 1\n" +
		"1: 00000000000000000000000001000000:1539 00:0000 0A 0:0 00:0 0 42 0 2\n" +
		"2: 00000000000000000000000001000000:153A 00:0000 0A 0:0 00:0 0 43 0 3\n")
	got := parseProcTCP(data, true, 42)
	if len(got) != 2 || got[0].Loopback != "::1" || got[1].Loopback != "::1" {
		t.Fatalf("services=%#v", got)
	}
}
