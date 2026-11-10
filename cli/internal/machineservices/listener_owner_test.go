package machineservices

import (
	"encoding/binary"
	"math/bits"
	"testing"
)

// networkPort encodes port the way GetExtendedTcpTable stores tcpRow4/6
// LocalPort: the port in network byte order (cf. windowsPort in
// discovery_windows.go).
func networkPort(port uint16) uint32 { return bits.ReverseBytes32(uint32(port) << 16) }

func appendRow4(buf []byte, state, addr, port uint32, pid uint32) []byte {
	row := make([]byte, 24)
	binary.LittleEndian.PutUint32(row[0:], state)
	binary.LittleEndian.PutUint32(row[4:], addr)
	binary.LittleEndian.PutUint32(row[8:], port)
	binary.LittleEndian.PutUint32(row[20:], pid)
	return append(buf, row...)
}

func appendRow6(buf []byte, addr [16]byte, port uint32, state, pid uint32) []byte {
	row := make([]byte, 56)
	copy(row[0:], addr[:])
	binary.LittleEndian.PutUint32(row[20:], port)
	binary.LittleEndian.PutUint32(row[48:], state)
	binary.LittleEndian.PutUint32(row[52:], pid)
	return append(buf, row...)
}

func TestListenPIDsOwnerPIDTable(t *testing.T) {
	cases := []struct {
		name string
		v6   bool
		rows func() []byte
		port uint16
		want []uint32
	}{
		{
			name: "v4 wildcard LISTEN on 80",
			port: 80,
			rows: func() []byte {
				buf := binary.LittleEndian.AppendUint32(nil, 4)
				buf = appendRow4(buf, 2, 0x00000000, networkPort(80), 1234)
				buf = appendRow4(buf, 2, 0x0100007f, networkPort(443), 1235)
				buf = appendRow4(buf, 5, 0x00000000, networkPort(80), 9)
				buf = appendRow4(buf, 2, 0x0101a8c0, networkPort(80), 77)
				return buf
			},
			want: []uint32{1234},
		},
		{
			name: "v4 loopback LISTEN on 443",
			port: 443,
			rows: func() []byte {
				buf := binary.LittleEndian.AppendUint32(nil, 2)
				buf = appendRow4(buf, 2, 0x0100007f, networkPort(443), 1235)
				buf = appendRow4(buf, 2, 0x00000000, networkPort(81), 78)
				return buf
			},
			want: []uint32{1235},
		},
		{
			name: "truncated table is no answer",
			port: 80,
			rows: func() []byte {
				buf := binary.LittleEndian.AppendUint32(nil, 3)
				return appendRow4(buf, 2, 0x00000000, networkPort(80), 1234)
			},
			want: nil,
		},
		{
			name: "v6 wildcard LISTEN on 443",
			v6:   true,
			port: 443,
			rows: func() []byte {
				buf := binary.LittleEndian.AppendUint32(nil, 3)
				buf = appendRow6(buf, [16]byte{}, networkPort(443), 2, 4321)
				buf = appendRow6(buf, [16]byte{15: 1}, networkPort(443), 2, 4322)
				buf = appendRow6(buf, [16]byte{}, networkPort(443), 5, 4323)
				return buf
			},
			want: []uint32{4321, 4322},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := listenPIDsOwnerPIDTable(c.rows(), c.v6, c.port)
			if len(got) != len(c.want) {
				t.Fatalf("pids=%v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("pids=%v want %v", got, c.want)
				}
			}
		})
	}
}

func TestListenInodesProcTCP(t *testing.T) {
	data := []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 0:0 00:0 0 0 0 11
   1: 0100007F:01BB 00000000:0000 0A 0:0 00:0 0 1000 0 12
   2: 0200007F:0050 00000000:0000 0A 0:0 00:0 0 0 0 13
   3: 0100007F:0050 00000000:0000 06 0:0 00:0 0 0 0 14
   4: 00000000:0051 00000000:0000 0A 0:0 00:0 0 0 0 15
`)
	if got := listenInodesProcTCP(data, false, 80); len(got) != 1 || got[0] != 11 {
		t.Fatalf("v4 port 80 inodes=%v", got)
	}
	if got := listenInodesProcTCP(data, false, 443); len(got) != 1 || got[0] != 12 {
		t.Fatalf("v4 port 443 inodes=%v", got)
	}
	data6 := []byte("0: 00000000000000000000000000000000:0050 00:0000 0A 0:0 00:0 0 0 0 21\n" +
		"1: 00000000000000000000000001000000:01BB 00:0000 0A 0:0 00:0 0 0 0 22\n" +
		"2: 00000000000000000000000000000000:0051 00:0000 0A 0:0 00:0 0 0 0 23\n")
	if got := listenInodesProcTCP(data6, true, 80); len(got) != 1 || got[0] != 21 {
		t.Fatalf("v6 port 80 inodes=%v", got)
	}
	if got := listenInodesProcTCP(data6, true, 443); len(got) != 1 || got[0] != 22 {
		t.Fatalf("v6 port 443 inodes=%v", got)
	}
}
