package machineguard

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pcbBuffer builds a pcblist_n buffer with one connection per tuple, owned by uid in state tcpState.
func pcbBuffer(inpcbLen int, conns ...[3]any) []byte {
	le := binary.LittleEndian
	gen := func(count uint32) []byte {
		b := make([]byte, 24)
		le.PutUint32(b, 24)
		le.PutUint32(b[4:], count)
		return b
	}
	out := gen(uint32(len(conns)))
	for i, c := range conns {
		tuple, uid, state := c[0].(socketTuple), c[1].(uint32), c[2].(uint32)
		inpcb := make([]byte, inpcbLen)
		le.PutUint32(inpcb, uint32(inpcbLen))
		le.PutUint32(inpcb[4:], 0x010)
		le.PutUint64(inpcb[8:], uint64(0x1000+i))
		binary.BigEndian.PutUint16(inpcb[16:], tuple.Destination.Port())
		binary.BigEndian.PutUint16(inpcb[18:], tuple.Source.Port())
		inpcb[44] = 0x1
		dst, src := tuple.Destination.Addr().As4(), tuple.Source.Addr().As4()
		copy(inpcb[60:], dst[:])
		copy(inpcb[76:], src[:])
		socket := make([]byte, 104)
		le.PutUint32(socket, 104)
		le.PutUint32(socket[4:], 0x001)
		le.PutUint64(socket[28:], uint64(0x1000+i))
		le.PutUint32(socket[64:], uid)
		buffers := make([]byte, 32)
		le.PutUint32(buffers, 32)
		le.PutUint32(buffers[4:], 0x002)
		tcpcb := make([]byte, 204)
		le.PutUint32(tcpcb, 204)
		le.PutUint32(tcpcb[4:], 0x020)
		le.PutUint32(tcpcb[36:], state)
		for _, r := range [][]byte{inpcb, socket, buffers, tcpcb} {
			out = append(out, r...)
			out = append(out, make([]byte, (8-len(r)%8)%8)...)
		}
	}
	return append(out, gen(0)...)
}

func TestParsePCBListAttributesOnlyAnExactKnownLayout(t *testing.T) {
	tuple := socketTuple{Source: netip.MustParseAddrPort("127.0.0.1:50000"), Destination: netip.MustParseAddrPort("127.100.0.1:443")}
	other := socketTuple{Source: netip.MustParseAddrPort("127.0.0.1:50001"), Destination: tuple.Destination}
	owners, err := parsePCBList(pcbBuffer(104, [3]any{other, uint32(501), uint32(4)}, [3]any{tuple, uint32(502), uint32(4)}), tuple)
	if err != nil || len(owners) != 1 || owners[0].UID != 502 || !owners[0].Established || owners[0].Tuple != tuple {
		t.Fatalf("owners=%+v err=%v; want one established owner 502", owners, err)
	}
	if owners, err := parsePCBList(pcbBuffer(104, [3]any{tuple, uint32(502), uint32(10)}), tuple); err != nil || len(owners) != 1 || owners[0].Established {
		t.Fatalf("TIME_WAIT: owners=%+v err=%v; want one not-established owner", owners, err)
	}
	if owners, err := parsePCBList(pcbBuffer(104, [3]any{other, uint32(501), uint32(4)}), tuple); err != nil || len(owners) != 0 {
		t.Fatalf("no match: owners=%+v err=%v", owners, err)
	}
	if _, err := parsePCBList(pcbBuffer(112, [3]any{tuple, uint32(502), uint32(4)}), tuple); !errors.Is(err, errPCBLayout) {
		t.Fatalf("an unknown inpcb length must fail closed, err=%v", err)
	}
	broken := pcbBuffer(104, [3]any{tuple, uint32(502), uint32(4)})
	binary.LittleEndian.PutUint64(broken[24+104+28:], 0xdead) // the socket's so_pcb no longer names its inpcb
	if _, err := parsePCBList(broken, tuple); !errors.Is(err, errPCBLayout) {
		t.Fatalf("a socket of another connection must fail closed, err=%v", err)
	}
	full := pcbBuffer(104, [3]any{tuple, uint32(502), uint32(4)})
	if _, err := parsePCBList(full[:len(full)-30], tuple); !errors.Is(err, errPCBLayout) {
		t.Fatalf("a truncated buffer must fail closed, err=%v", err)
	}
}

func TestSnapshotFlightServesConcurrentLookupsFromOneScan(t *testing.T) {
	var scans atomic.Int32
	release := make(chan struct{})
	flight := &snapshotFlight{fetch: func() ([]byte, error) {
		scans.Add(1)
		<-release
		return []byte("snapshot"), nil
	}}
	const waiters = 50
	var wg sync.WaitGroup
	results := make(chan string, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf, err := flight.get()
			if err != nil {
				t.Error(err)
			}
			results <- string(buf)
		}()
	}
	for scans.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let every waiter join the in-flight scan
	close(release)
	wg.Wait()
	close(results)
	for r := range results {
		if r != "snapshot" {
			t.Fatalf("a waiter got %q", r)
		}
	}
	if n := scans.Load(); n != 1 {
		t.Fatalf("%d concurrent lookups took %d scans; want 1", waiters, n)
	}
	if _, err := flight.get(); err != nil || scans.Load() != 2 {
		t.Fatalf("a lookup after the burst must take a fresh scan: scans=%d err=%v", scans.Load(), err)
	}
}
