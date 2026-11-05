package machineguard

import (
	"encoding/binary"
	"errors"
	"net/netip"
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
		genLen                        = 24  // sizeof(struct xinpgen)
		inpcbLen, socketLen, tcpcbLen = 104, 104, 204
		inpIPv4                       = 0x1 // INP_IPV4 in inp_vflag
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
		if inpcb[44]&inpIPv4 != 0 {
			src := netip.AddrPortFrom(netip.AddrFrom4([4]byte(inpcb[76:80])), binary.BigEndian.Uint16(inpcb[18:]))
			dst := netip.AddrPortFrom(netip.AddrFrom4([4]byte(inpcb[60:64])), binary.BigEndian.Uint16(inpcb[16:]))
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
