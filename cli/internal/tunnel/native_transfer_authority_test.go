package tunnel

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestNativeTransferUsesOwnIdentityBeforeAuthenticatedRemoteNetwork(t *testing.T) {
	const issuer = "https://api.example.test"
	store := connectedStore(t)
	if err := store.Save(config.Profile{Issuer: issuer, Account: config.Account{ID: "account_connected"}, CLIClientSessionID: "cli_connected", AccessExpiresAt: time.Now().Add(time.Hour)}, config.Credential{AccessToken: "machine-test-token", RefreshToken: "refresh-test-token"}); err != nil {
		t.Fatal(err)
	}
	identity, _, _ := connectedEndpointAuthority(t, store, issuer, "account_connected", "cli_connected", "unused_machine")
	defer identity.Clear()
	var targetReads, networkReads atomic.Int32
	control := &connectedMachineAccess{}
	transport := connectedRoundTripper(func(request *http.Request) (*http.Response, error) {
		status := http.StatusServiceUnavailable
		switch request.URL.Path {
		case "/v1/endpoints/machine_foreign/certificates/1":
			targetReads.Add(1)
			status = http.StatusNotFound
		case "/.well-known/jwks.json":
			networkReads.Add(1)
		default:
			return control.control(request, identity, connectedNetworkAPI{})
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unavailable"}}`))}, nil
	})
	peer, err := NewPeerTerminalTunnel(PeerTerminalConfig{Issuer: issuer, Store: store, Auth: &rotatingNativeAuth{token: "machine-test-token"}, TLS: &tls.Config{}, HTTPClient: &http.Client{Transport: transport, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = peer.PrepareNativeFileTransfer(ctx, resolver.ConnectInfo{Workspace: "paid-team", TargetKind: "machine", MachineID: "machine_foreign", MachineGeneration: 1, Terminal: &resolver.TerminalTarget{EnvironmentID: "environment_foreign", Auth: resolver.AuthTarget{ResourceID: "session_shared", Token: "signed-transfer-grant"}}}, "operation_shared")
	if err == nil || targetReads.Load() != 0 || networkReads.Load() != 1 {
		t.Fatalf("shared transfer requested a same-account target certificate instead of reaching network verification: target_reads=%d network_reads=%d err=%v", targetReads.Load(), networkReads.Load(), err)
	}
}
