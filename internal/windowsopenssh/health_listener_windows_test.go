//go:build windows

package windowsopenssh

import (
	"encoding/binary"
	"golang.org/x/sys/windows"
	"testing"
)

func TestNativeListenerTableDecodesBothFamiliesAndRejectsMalformedRows(t *testing.T) {
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		size, portAt, stateAt, pidAt, address := 24, 8, 0, 20, "127.0.0.1"
		if family == windows.AF_INET6 {
			size, portAt, stateAt, pidAt, address = 56, 20, 48, 52, "::1"
		}
		table := make([]byte, 4+size)
		binary.LittleEndian.PutUint32(table, 1)
		row := table[4:]
		if family == windows.AF_INET {
			copy(row[4:8], []byte{127, 0, 0, 1})
		} else {
			row[15] = 1
		}
		binary.BigEndian.PutUint16(row[portAt:portAt+2], 41239)
		binary.LittleEndian.PutUint32(row[stateAt:stateAt+4], 2)
		binary.LittleEndian.PutUint32(row[pidAt:pidAt+4], 1234)
		got, err := decodeNativeListenerTable(table, family, 41239)
		if err != nil || len(got) != 1 || got[0].Address != address || got[0].Port != 41239 || got[0].ProcessID != 1234 {
			t.Fatalf("family %d row=%+v err=%v", family, got, err)
		}
		if other, err := decodeNativeListenerTable(table, family, 41240); err != nil || len(other) != 0 {
			t.Fatal("unrelated port included")
		}
		if _, err := decodeNativeListenerTable(table[:len(table)-1], family, 41239); err == nil {
			t.Fatal("truncated row accepted")
		}
		binary.LittleEndian.PutUint32(table, 0xffffffff)
		if _, err := decodeNativeListenerTable(table, family, 41239); err == nil {
			t.Fatal("unchecked count accepted")
		}
		binary.LittleEndian.PutUint32(table, 1)
		binary.LittleEndian.PutUint32(row[pidAt:pidAt+4], 0)
		if _, err := decodeNativeListenerTable(table, family, 41239); err == nil {
			t.Fatal("ownerless listener accepted")
		}
	}
}
