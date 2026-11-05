package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// F21-02: a private preview/tunnel binding carries the server's expiry. With the host's clock 10 min ahead,
// a binding with a minute left was refused before the target dial; 10 min behind, an expired one was let in.
func TestNativePrivateBindingIsJudgedOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	for name, hostAhead := range map[string]time.Duration{"host 10 min ahead": 10 * time.Minute, "host 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverclock.Observe(time.Now().Add(-hostAhead).UTC().Format(http.TimeFormat), time.Now(), time.Now())
			for state, expires := range map[string]time.Time{"fresh": serverclock.Now().Add(time.Minute), "expired": serverclock.Now().Add(-time.Minute)} {
				binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 1, RouteID: "route_1", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: expires.UTC()}
				target, _ := json.Marshal(binding)
				if _, err := nativeprivate.Decode(target, serverclock.Now()); state == "expired" && !errors.Is(err, nativeprivate.ErrExpired) {
					t.Fatalf("expired binding error = %v, want nativeprivate.ErrExpired", err)
				}
				header := streamauth.Header{Version: streamauth.Version, OperationID: "operation_clock", Consumer: "private_tcp", StreamID: "stream_clock", Credential: "credential", DeadlineUnix: serverclock.Now().Add(time.Minute).Unix(), MaximumBytes: 1024, Target: string(target)}
				client, peer := net.Pipe()
				dialed := errors.New("target dial reached")
				err := ServeNativePrivateTCP(context.Background(), header, client, func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
					return expires, nil, nil
				}, func(context.Context, string, string) (net.Conn, error) { return nil, dialed })
				_, _ = client.Close(), peer.Close()
				if state == "fresh" && !errors.Is(err, dialed) {
					t.Fatalf("fresh binding refused before the dial: %v", err)
				}
				if state == "expired" && !errors.Is(err, ErrNativePrivateBinding) {
					t.Fatalf("expired binding was not refused: %v", err)
				}
			}
		})
	}
}
