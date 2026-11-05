//go:build darwin

package machineguard

import (
	"net"
	"os"
	"testing"
)

// TestPCBListAttributesALiveLoopbackConnectionToItsUID runs against the real kernel: the connecting socket of a
// loopback connection this test opened is owned by the test's uid and established.
func TestPCBListAttributesALiveLoopbackConnectionToItsUID(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	tuple, err := clientTuple(accepted)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := attributeOwner(tuple, lookupOwnersPCBList)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Fatalf("attributed uid=%d err=%v; want %d", uid, err, os.Getuid())
	}
}
