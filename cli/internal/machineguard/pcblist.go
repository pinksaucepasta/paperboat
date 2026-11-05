package machineguard

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
)

// parsePCBList finds the sockets with tuple in a macOS net.inet.tcp.pcblist_n buffer (F23-01/F23-02).
// The buffer is walked the way netstat does it (Apple network_cmds netstat.tproj/inet.c protopr): an xinpgen
// header, then records each led by its own length and kind, 8-byte aligned, until a trailing xinpgen; one
// connection is an XSO_INPCB record followed by its XSO_SOCKET, XSO_TCPCB and buffer records. The fields read
// inside a record follow xnu's xinpcb_n (bsd/netinet/in_pcb.h), xsocket_n (bsd/sys/socketvar.h) and xtcpcb_n
// (bsd/netinet/tcp_var.h), checked against a live connection on macOS 26. Those layouts are not an API, so a
// record of any other length, a broken buffer or a socket that does not belong to its connection is an error:
// no attribution rather than a guess.
func parsePCBList(buf []byte, tuple socketTuple) ([]socketOwner, error) {
	const (
		xsoSocket, xsoInpcb, xsoTCPCB = 0x001, 0x010, 0x020
		genLen                        = 24 // sizeof(struct xinpgen)
		inpcbLen, socketLen, tcpcbLen = 104, 104, 204
		inpIPv4, inpIPv6              = 0x1, 0x2 // INP_IPV4, INP_IPV6 in inp_vflag
		tcpsEstablished               = 4
	)
	le := binary.LittleEndian
	if len(buf) < genLen || le.Uint32(buf) != genLen {
		return nil, errPCBLayout
	}
	var owners []socketOwner
	var inpcb, socket, tcpcb []byte
	flush := func() error {
		if inpcb == nil {
			return nil
		}
		if socket == nil || tcpcb == nil || le.Uint64(socket[28:]) != le.Uint64(inpcb[8:]) {
			return errPCBLayout
		}
		// inp_dependfaddr / inp_dependladdr are 16-byte unions at 48 and 64: an IPv6 address, or an IPv4 one in
		// the last 4 bytes (xnu in_pcb.h). The IPv6 case is read from the same offsets; it is unverified on a
		// live Mac (the IPv4 layout was checked on macOS 26), so the macOS lane runs the ::1 check.
		var src, dst netip.AddrPort
		known := true
		switch {
		case inpcb[44]&inpIPv4 != 0:
			src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(inpcb[76:80])), binary.BigEndian.Uint16(inpcb[18:]))
			dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(inpcb[60:64])), binary.BigEndian.Uint16(inpcb[16:]))
		case inpcb[44]&inpIPv6 != 0:
			src = netip.AddrPortFrom(netip.AddrFrom16([16]byte(inpcb[64:80])), binary.BigEndian.Uint16(inpcb[18:]))
			dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(inpcb[48:64])), binary.BigEndian.Uint16(inpcb[16:]))
		default:
			known = false
		}
		if known {
			if (socketTuple{Source: src, Destination: dst}) == tuple {
				owners = append(owners, socketOwner{Tuple: tuple, UID: le.Uint32(socket[64:]), Established: le.Uint32(tcpcb[36:]) == tcpsEstablished})
			}
		}
		inpcb, socket, tcpcb = nil, nil, nil
		return nil
	}
	for off := genLen; ; {
		if off+8 > len(buf) {
			return nil, errPCBLayout
		}
		n, kind := int(le.Uint32(buf[off:])), le.Uint32(buf[off+4:])
		if n == genLen { // the trailing xinpgen
			if err := flush(); err != nil {
				return nil, err
			}
			return owners, nil
		}
		if n < 8 || off+n > len(buf) {
			return nil, errPCBLayout
		}
		record := buf[off : off+n]
		switch kind {
		case xsoInpcb:
			if err := flush(); err != nil {
				return nil, err
			}
			if n != inpcbLen {
				return nil, errPCBLayout
			}
			inpcb = record
		case xsoSocket:
			if n != socketLen || inpcb == nil || socket != nil {
				return nil, errPCBLayout
			}
			socket = record
		case xsoTCPCB:
			if n != tcpcbLen || inpcb == nil || tcpcb != nil {
				return nil, errPCBLayout
			}
			tcpcb = record
		}
		off += (n + 7) &^ 7
	}
}

var errPCBLayout = errors.New("unrecognized macOS pcblist_n layout")

// snapshotFlight coalesces concurrent kernel snapshots (singleflight): every lookup that arrives while a snapshot
// is being taken shares it, so a burst of new gateway connections costs one pcb-list scan, not one each. A
// connection accepted just after that snapshot began may be missing from it; attributeOwner then retries, and the
// retry takes a fresh snapshot.
type snapshotFlight struct {
	mu    sync.Mutex
	call  *snapshotCall
	fetch func() ([]byte, error)
}

type snapshotCall struct {
	done chan struct{}
	buf  []byte
	err  error
}

func (f *snapshotFlight) get() ([]byte, error) {
	f.mu.Lock()
	if call := f.call; call != nil {
		f.mu.Unlock()
		<-call.done
		return call.buf, call.err
	}
	call := &snapshotCall{done: make(chan struct{})}
	f.call = call
	f.mu.Unlock()
	call.buf, call.err = f.fetch()
	f.mu.Lock()
	f.call = nil
	f.mu.Unlock()
	close(call.done)
	return call.buf, call.err
}
