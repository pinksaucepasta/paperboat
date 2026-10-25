package connector

import (
	"context"
	"net"
	"testing"
)

// A failed yamux open or accept must return an untyped nil link: a nil *yamux.Stream inside the
// interface looked non-nil, and closing it after a canceled open crashed the daemon (CI run
// 40244428401, a goaway racing carrier shutdown).
func TestYamuxSessionFailuresReturnNoLink(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	session, err := newYamuxDataCarrierSession(local, DefaultDataCarrierConfig(), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if link, err := session.OpenStream(context.Background()); err == nil || link != nil {
		t.Fatalf("open on a closed session = %v, %v", link, err)
	}
	if link, err := session.AcceptStream(context.Background()); err == nil || link != nil {
		t.Fatalf("accept on a closed session = %v, %v", link, err)
	}
}
