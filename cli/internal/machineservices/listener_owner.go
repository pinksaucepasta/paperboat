package machineservices

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// ListenerOwner reports which local program is LISTENing on 127.0.0.1:port or
// on a wildcard (0.0.0.0, ::) that covers it, so pb doctor can name the
// blocker of gateway ports 80/443. ok is false when nothing LISTENs there or
// the owner cannot be identified. A name of "" with ok == true means the pid
// is known but its executable is not readable (another OS user's process):
// callers render that as "unknown program". Only the executable base name is
// ever returned, never a path or command line.
func ListenerOwner(port uint16) (name string, pid int, ok bool) {
	if port == 0 {
		return "", 0, false
	}
	return listenerOwner(port)
}

// tcpListenState is MIB_TCP_STATE_LISTEN, cf. tcpStateListen in
// discovery_windows.go (which that file's windows-only build can see; this
// file stays portable so its tests run on Linux).
const tcpListenState = 2

// maxListenerOwners caps every bounded scan in this helper.
const maxListenerOwners = 64

// listenPIDsOwnerPIDTable returns the pids of LISTEN rows for port in a
// TCP_TABLE_OWNER_PID_ALL buffer (GetExtendedTcpTable): a little-endian u32
// count followed by tcpRow4 (v6 == false) or tcpRow6 (v6 == true) rows, the
// layout read by readTCPTable in discovery_windows.go. Ports are stored with
// the port in network byte order, decoded the way windowsPort does. Only
// wildcard and loopback local addresses are accepted: a LISTEN anywhere else
// cannot block 127.0.0.1:port. A truncated or absurd count is not an error,
// just no answer.
func listenPIDsOwnerPIDTable(data []byte, v6 bool, port uint16) []uint32 {
	rowLen := 24
	if v6 {
		rowLen = 56
	}
	if len(data) < 4 {
		return nil
	}
	count := binary.LittleEndian.Uint32(data)
	if uint64(count) > uint64(len(data)-4)/uint64(rowLen) || count > 1<<20 {
		return nil
	}
	var pids []uint32
	for i := uint32(0); i < count; i++ {
		row := data[4+int(i)*rowLen:]
		var st, portField, pid uint32
		var wildcardOrLoopback bool
		if v6 {
			portField = binary.LittleEndian.Uint32(row[20:])
			st = binary.LittleEndian.Uint32(row[48:])
			pid = binary.LittleEndian.Uint32(row[52:])
			addr := netip.AddrFrom16([16]byte(row[0:16])).Unmap()
			wildcardOrLoopback = addr.IsUnspecified() || addr == netip.IPv6Loopback()
		} else {
			portField = binary.LittleEndian.Uint32(row[8:])
			st = binary.LittleEndian.Uint32(row[0:])
			pid = binary.LittleEndian.Uint32(row[20:])
			addr := bits.ReverseBytes32(binary.LittleEndian.Uint32(row[4:]))
			wildcardOrLoopback = addr == 0 || addr == 0x7f000001
		}
		if st != tcpListenState || uint16(bits.ReverseBytes32(portField)>>16) != port || !wildcardOrLoopback || pid == 0 {
			continue
		}
		pids = append(pids, pid)
		if len(pids) >= maxListenerOwners {
			break
		}
	}
	return pids
}

// listenInodesProcTCP returns the socket inodes of LISTEN rows for port in
// /proc/net/tcp (ipv6 == false) or /proc/net/tcp6 data, accepting only
// wildcard and canonical-loopback local addresses: the same sets parseProcTCP
// in discovery_linux.go accepts, but without its owner-UID filter, because
// the blocker is usually another OS user's program. Fields mirror
// parseProcTCP: fields[1] is local_address:port (port hex), fields[3] is st,
// fields[9] is the inode.
func listenInodesProcTCP(data []byte, ipv6 bool, port uint16) []uint64 {
	var inodes []uint64
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		address, portText, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		portValue, err := strconv.ParseUint(portText, 16, 16)
		if err != nil || uint16(portValue) != port {
			continue
		}
		loopback := false
		if ipv6 {
			decoded, err := hex.DecodeString(address)
			if err != nil || len(decoded) != 16 {
				continue
			}
			// Same two forms parseProcTCP in discovery_linux.go accepts.
			loopback = bytes.Equal(decoded, make([]byte, 16)) ||
				bytes.Equal(decoded, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0})
		} else {
			loopback = address == "00000000" || address == "0100007F"
		}
		if !loopback {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		inodes = append(inodes, inode)
		if len(inodes) >= maxListenerOwners {
			break
		}
	}
	return inodes
}
