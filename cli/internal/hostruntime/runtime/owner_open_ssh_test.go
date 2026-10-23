//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

type ownerTestGate struct{}

func (ownerTestGate) HandleUpdateGate(context.Context, hostdproto.UpdateGateRequest) (hostdproto.UpdateGateResponse, error) {
	return hostdproto.UpdateGateResponse{}, nil
}

// hostd asks the worker for the SSH connections it bridges itself; only the owner capability may ask.
func TestOwnerReadsTheWorkersOpenSSHCount(t *testing.T) {
	mux := http.NewServeMux()
	host := &Host{handler: mux, updateGate: ownerTestGate{}}
	capability := bytes.Repeat([]byte{7}, 32)
	if err := host.BindOwnerUpdateGate(capability); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	listen := strings.TrimPrefix(server.URL, "http://")
	owner := &ProcessOwner{listen: listen, token: capability}
	if open, err := owner.OpenSSHStreams(context.Background()); err != nil || open != 0 {
		t.Fatalf("open SSH = %d, %v", open, err)
	}
	stranger := &ProcessOwner{listen: listen, token: bytes.Repeat([]byte{8}, 32)}
	if _, err := stranger.OpenSSHStreams(context.Background()); err == nil {
		t.Fatal("a caller without the owner capability read the count")
	}
}
