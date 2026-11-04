package tailnet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/quic-go/quic-go"
)

func (c *CarrierNetwork) RelayNodeIDs() []string {
	c.authority.relay.mu.Lock()
	defer c.authority.relay.mu.Unlock()
	var ids []string
	for _, node := range c.authority.relay.nodes {
		grant := c.authority.relay.grants[node.NodeID]
		if grant.ExpiresAt > time.Now().Unix() && grant.ProcessEpoch == node.ProcessEpoch && grant.NodeGeneration == node.NodeGeneration {
			ids = append(ids, node.NodeID)
		}
	}
	sort.Strings(ids)
	return ids
}

// The signed target offer and our cached grants determine the common relays,
// preferred first. The old connection's selection is a preference, never extra
// authority. A dead or refusing relay is skipped by the caller (each account
// has two edges).
func (c *CarrierNetwork) CommonRelays(remote []string, preferred string) []string {
	var out []string
	for _, id := range c.RelayNodeIDs() {
		for _, other := range remote {
			if id == other {
				if id == preferred {
					out = append([]string{id}, out...)
				} else {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// Select per peer, retaining all other peers' active relay routes. Candidate
// signalling over the old connection lets both sides connect to this same
// authorized node without a control-plane request or peer fan-out.
func (c *CarrierNetwork) PreparePeerRelay(ctx context.Context, endpointID, nodeID string) error {
	if nodeID == "" {
		return nil
	}
	allowed := false
	for _, id := range c.RelayNodeIDs() {
		allowed = allowed || id == nodeID
	}
	if !allowed {
		return ErrRegionalAuthority
	}
	binding, err := c.authority.Peer(endpointID)
	if err != nil {
		return err
	}
	peer, err := publicKey(binding.WireGuardPublicKey)
	if err != nil {
		return err
	}
	id := regionalID(nodeID)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrAuthority
	}
	err = c.server.SetPeerRelayRegion(peer, id)
	c.mu.Unlock()
	if err == nil {
		err = c.server.PrepareRelay(ctx, id)
	}
	if err != nil {
		// Never a silent relay failure: name the relay's own close code.
		diagnosticlog.TryInfo("native packet lifecycle", "stage", "relay_prepare", "failed", true, "error_type", relayFailureType(err))
	}
	return err
}

// relayFailureType is bounded metadata: the relay's QUIC application close
// code (derpquic Error.Code: 1 admission, 2 protocol, 3 overload, 4 expired,
// 5 quota) or the Go error type.
func relayFailureType(err error) string {
	var relay *derpquic.Error
	if errors.As(err, &relay) {
		return fmt.Sprintf("relay_refused_%d", relay.Code)
	}
	var app *quic.ApplicationError
	if errors.As(err, &app) {
		return fmt.Sprintf("relay_close_%d", app.ErrorCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", err), "*")
}
