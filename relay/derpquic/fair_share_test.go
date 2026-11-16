package derpquic

import (
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"tailscale.com/types/key"
)

// Under pressure (half full) an account that holds its equal share of the node gets no more connections, so a busy
// account cannot take what other accounts need; a node below half full applies nothing, and an account holding less
// than its share still connects.
func TestAccountsShareAFullNodeEqually(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	s.connectionLimit = 20
	hold := func(account string, n int) {
		for range n {
			s.peers[key.NewNode().Public()] = &peerConn{grant: Grant{RelayGrant: peernetwork.RelayGrant{AccountID: account}}}
		}
	}
	admit := func(account string) error {
		g := f.grant(key.NewNode().Public(), certificate(t), "endpoint-"+account)
		g.AccountID = account
		return s.admit(&peerConn{}, g, time.Now())
	}
	hold("big", 12)
	if err := admit("big"); errors.Is(err, ErrOverload) {
		t.Fatalf("a lone account on a node below half full: %v", err)
	}
	hold("other", 3)
	// A reconnect of an endpoint the account already holds takes no new slot: not refused though it is over its share.
	reconnectKey := key.NewNode().Public()
	s.peers[reconnectKey] = &peerConn{grant: Grant{RelayGrant: peernetwork.RelayGrant{AccountID: "big", EndpointID: "reconnecting"}}}
	reconnect := f.grant(reconnectKey, certificate(t), "reconnecting")
	reconnect.AccountID = "big"
	if err := s.admit(&peerConn{}, reconnect, time.Now()); errors.Is(err, ErrOverload) {
		t.Fatalf("a reconnect of a held endpoint was refused by the fair share: %v", err)
	}
	if err := admit("big"); !errors.Is(err, ErrOverload) {
		t.Fatalf("an account holding more than its equal share (12 of 20, two accounts) was admitted: %v", err)
	}
	if err := admit("other"); errors.Is(err, ErrOverload) {
		t.Fatalf("an account below its equal share was refused: %v", err)
	}
	if err := admit("third"); errors.Is(err, ErrOverload) {
		t.Fatalf("a new account was refused on a node with room: %v", err)
	}
}
