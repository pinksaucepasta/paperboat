package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=relay-credential-test
	"tailscale.com/tailcfg"
)

// A new relay dial that meets an expired grant (10-minute grants now, so after a long control outage) must ask for
// a fresh grant and retry. ErrRelayGrantExpired was a plain error, so derpquic.Client latched it as a final
// admission refusal and that relay region stayed dead until the engine was rebuilt.
func TestExpiredRelayGrantIsRefetchedAndRetriedNotLatched(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	node := RegionalNode{NodeID: "regional-expired", NodeGeneration: 1, ProcessEpoch: "epoch-expired"}
	a.relay.mu.Lock()
	a.relay.nodes = map[tailcfg.DERPRegionID]RegionalNode{regionalID(node.NodeID): node}
	a.relay.grants = map[string]peernetwork.RelayGrant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: 1, ProcessEpoch: "epoch-expired", Generation: 1, ExpiresAt: serverclock.Now().Unix() - 1}}
	a.relay.tokens = map[string]string{node.NodeID: "grant-expired"}
	a.relay.mu.Unlock()
	credential := a.relayCredential(node)
	calls := 0
	client := derpquic.NewClient(derpquic.ClientConfig{Address: "127.0.0.1:1", TLS: &tls.Config{Certificates: []tls.Certificate{{}}}, Credential: func(ctx context.Context) (string, error) {
		calls++
		return credential(ctx)
	}})
	defer client.Close()
	for attempt := range 2 {
		err := client.Connect(t.Context())
		var fatal interface{ Fatal() bool }
		if !errors.Is(err, derpquic.ErrExpired) || errors.As(err, &fatal) && fatal.Fatal() {
			t.Fatalf("attempt %d: an expired grant was refused as final: %v", attempt, err)
		}
	}
	if calls != 2 {
		t.Fatalf("the second dial asked the credential %d times in all, want 2: the client latched the refusal", calls)
	}
	select {
	case <-a.refreshNow:
	default:
		t.Fatal("an expired grant did not ask for a fresh one")
	}
}
