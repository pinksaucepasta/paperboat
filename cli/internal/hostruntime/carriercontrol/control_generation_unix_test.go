//go:build linux || darwin

package carriercontrol

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// An update reads the candidate generation's offer and an older generation
// routes a moving client's candidates to it, both over this socket. A
// generation that is not delegated answers unavailable.
func TestGenerationOfferAndPreparePeerRoundTrip(t *testing.T) {
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	token, sha := []byte(strings.Repeat("c", 32)), strings.Repeat("a", 64)
	offer := bytes.Repeat([]byte("o"), 6<<10)
	var gotPeer string
	var gotExchange []byte
	server := &Server{Endpoint: filepath.Join(root, "control.sock"), Token: token, SHA256: sha, SSH: ssh, Ready: make(chan error, 1),
		GenerationOffer: func() ([]byte, error) { return offer, nil },
		PreparePeer: func(_ context.Context, peer string, exchange []byte) ([]byte, error) {
			gotPeer, gotExchange = peer, exchange
			return offer, nil
		}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.Endpoint, token, sha)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.GenerationOffer(ctx); err != nil || !bytes.Equal(got, offer) {
		t.Fatal("generation offer did not round trip", err)
	}
	exchange := bytes.Repeat([]byte("e"), 10<<10)
	if got, err := client.PreparePeer(ctx, "cli_test", exchange); err != nil || !bytes.Equal(got, offer) || gotPeer != "cli_test" || !bytes.Equal(gotExchange, exchange) {
		t.Fatal("prepare-peer did not round trip", err)
	}
	if _, err := client.PreparePeer(ctx, "", exchange); !errors.Is(err, ErrInvalid) {
		t.Fatal("prepare-peer without a peer was sent", err)
	}
	server.GenerationOffer = nil
	if _, err := client.GenerationOffer(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("a primary generation answered an offer", err)
	}
	cancel()
	<-done
}
