package neterr

import (
	"net"
	"testing"
)

// A real refused dial is the case that matters: on Windows its error is a Winsock code.
func TestRefusedRecognisesARealRefusedDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = net.Dial("tcp", address)
	if err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if !Refused(err) {
		t.Fatalf("a refused dial was not recognised: %v", err)
	}
	if Reset(err) || Aborted(err) {
		t.Fatalf("a refused dial was classified as reset or aborted: %v", err)
	}
	if Refused(nil) || Refused(net.ErrClosed) {
		t.Fatal("unrelated errors were classified as refused")
	}
}
