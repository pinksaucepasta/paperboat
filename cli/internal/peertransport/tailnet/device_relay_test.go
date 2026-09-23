package tailnet

import (
	"net/netip"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"tailscale.com/types/key"
)

func TestDeviceRelayAuthoritativeEmptySetDoesNotAllocateListener(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureDeviceRelay([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:41642")}); err != nil {
		t.Fatal(err)
	}
	if a.relay.device != nil {
		t.Fatal("authoritative empty relay-pair set allocated a device-relay listener")
	}
}

func TestDeviceRelayPairAuthorityFencesSourceAndRefresh(t *testing.T) {
	firstNode, secondNode, stranger := key.NewNode().Public(), key.NewNode().Public(), key.NewNode().Public()
	firstDisco, secondDisco := key.NewDisco().Public(), key.NewDisco().Public()
	pair := RelayPair{ResourceKind: "machine_access", ResourceID: "access_1", ResourceGeneration: 1, ExpiresAt: time.Now().Add(time.Minute).Unix(), First: NetworkBinding{AccountID: "account", WireGuardPublicKey: derpquic.KeyString(firstNode), DiscoPublicKey: derpquic.DiscoKeyString(firstDisco)}, Second: NetworkBinding{AccountID: "account", WireGuardPublicKey: derpquic.KeyString(secondNode), DiscoPublicKey: derpquic.DiscoKeyString(secondDisco)}}
	device := &deviceRelay{pairs: []RelayPair{pair}}
	request := deviceRelayRequest{device: device, peer: firstNode}
	if source, err := request.SourceDisco(); err != nil || source != firstDisco {
		t.Fatalf("source=%v err=%v", source, err)
	}
	lease, err := request.AuthorizePair(firstDisco, secondDisco)
	if err != nil {
		t.Fatal(err)
	}
	if _, valid := lease.Valid(); !valid {
		t.Fatal("fresh signed pair lease is invalid")
	}
	request.peer = stranger
	if _, err := request.AuthorizePair(firstDisco, secondDisco); err == nil {
		t.Fatal("unlisted source authorized a pair")
	}
	device.update(nil)
	if _, valid := lease.Valid(); valid {
		t.Fatal("removed signed pair remained valid")
	}
}
