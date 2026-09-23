package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserPreviewAdmissionBeforeDialAndActiveExpiry(t *testing.T) {
	for _, mode := range []string{"allowed", "viewer", "host", "path", "target", "publication", "control-loss", "expiry", "unix", "team", "team-raw", "renewal"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var hits, dials, lookups atomic.Int32
			refreshed := make(chan struct{})
			originStopped := make(chan struct{}, 1)
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if mode == "renewal" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					select {
					case <-refreshed:
						_, _ = io.WriteString(w, "browser-ok")
					case <-r.Context().Done():
					}
					return
				}
				if mode == "expiry" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					originStopped <- struct{}{}
					return
				}
				_, _ = io.WriteString(w, "browser-ok")
			}))
			if mode == "unix" {
				if runtime.GOOS == "windows" {
					t.Skip("Unix socket runtime check")
				}
				directory, err := os.MkdirTemp("", "pb-browser-unix-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(directory)
				socket, err := net.Listen("unix", filepath.Join(directory, "origin.sock"))
				if err != nil {
					t.Fatal(err)
				}
				_ = origin.Listener.Close()
				origin.Listener = socket
			}
			origin.Start()
			defer origin.Close()
			identity := testPreviewCarrierIdentity(1)
			pair := newPreviewCarrierPair(t, ctx, identity)
			defer pair.close()
			hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			lease := Lease{ID: "preview-browser", AccountID: identity.AccountID, Generation: 1, AccessMode: "private", Endpoint: "https://browser.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}}
			if mode == "unix" {
				lease.Target.Scheme = "unix"
			}
			if mode == "renewal" {
				lease.Generation = 17
			}
			if mode == "team" || mode == "team-raw" {
				lease.AccessMode = "team"
			}
			now := time.Now()
			d := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "env-browser", AccountID: identity.AccountID, TunnelID: identity.TunnelID, Lifecycle: "ephemeral", ResourceGeneration: 1, RouteID: lease.ID, RouteGeneration: 1, TargetID: lease.ID, TargetGeneration: 1, HostID: identity.HostID, InstallationGeneration: 1, Audience: lease.AccessMode, ConnectionMethod: "edge", Protocol: "http", Hostname: "browser.example.test", PathPrefix: "/", OriginScheme: lease.Target.Scheme, OriginAddress: lease.Target.Address, TLSVerification: "not_applicable", PublicationID: lease.ID, PublicationGeneration: 1}, DecisionID: "decision-browser", PolicyGeneration: 1, EdgeNodeID: "edge-browser", EdgeProcessEpoch: "epoch-1234567890123456789012345678", ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, AssignmentGeneration: 1, PrincipalID: "viewer-browser", GrantID: "grant-browser", GrantGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
			if lease.AccessMode == "team" {
				d.MembershipGeneration = 1
			}
			if mode == "expiry" {
				d.ExpiresAt = now.Add(time.Second)
			}
			authority := d
			if mode == "viewer" {
				d.PrincipalID = "viewer-other"
			}
			if mode == "target" {
				d.Binding.OriginAddress = "127.0.0.1:1"
				authority = d
			}
			if mode == "publication" {
				d.Binding.PublicationID = "preview-other"
				authority = d
			}
			carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity, BrowserRouteGeneration: 1, DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
				dials.Add(1)
				network := "tcp"
				if target.Scheme == "unix" {
					network = "unix"
				}
				return (&net.Dialer{}).DialContext(ctx, network, target.Address)
			}, BrowserIngress: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
				if mode == "control-loss" {
					return connectorprotocol.IngressDecision{}, errors.New("unavailable")
				}
				if mode == "renewal" {
					current := authority
					current.IssuedAt = time.Now()
					current.ExpiresAt = current.IssuedAt.Add(10 * time.Second)
					if lookups.Add(1) == 2 {
						close(refreshed)
					}
					return current, nil
				}
				return authority, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{})
			done := make(chan error, 1)
			go func() { done <- carrier.Run(ctx, lease, func(Lease) error { close(ready); return nil }) }()
			defer func() {
				cancel()
				_ = carrier.Close(context.Background())
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("carrier did not stop")
				}
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("not ready")
			}
			baseline := dials.Load()
			stream, err := pair.edge.OpenStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			open := testPreviewStreamOpen(identity, lease.ID, "request-browser")
			open.Kind = "http_browser"
			if mode == "team-raw" {
				open.Kind = "http"
			}
			if err = connectorprotocol.WriteStreamOpen(stream, open); err != nil {
				t.Fatal(err)
			}
			if err = connectorprotocol.WriteIngressDecision(stream, d, time.Now()); err != nil && mode != "team-raw" {
				t.Fatal(err)
			}
			host, path := "browser.example.test", "/hello"
			if mode == "host" {
				host = "other.example.test"
			}
			if mode == "path" {
				path = "/a/../admin"
			}
			_, _ = fmt.Fprintf(stream, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host)
			response, err := http.ReadResponse(bufio.NewReader(stream), nil)
			allowed := mode == "allowed" || mode == "expiry" || mode == "unix" || mode == "team" || mode == "renewal"
			if !allowed {
				if err == nil {
					response.Body.Close()
					t.Fatal("denied request returned response")
				}
				if hits.Load() != 0 || dials.Load() != baseline {
					t.Fatalf("denied request dialed origin: hits=%d dials=%d baseline=%d", hits.Load(), dials.Load(), baseline)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if mode == "allowed" || mode == "unix" || mode == "team" || mode == "renewal" {
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "browser-ok" {
					t.Fatalf("body=%q error=%v", body, err)
				}
			} else {
				_, _ = io.ReadAll(response.Body)
				select {
				case <-originStopped:
				case <-ctx.Done():
					t.Fatal("expiry did not cancel origin")
				}
			}
		})
	}
}
