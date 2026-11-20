// Package relaylimits holds the grant timings that the control plane and the relay both enforce.
package relaylimits

import "time"

// MaxGrantLifetime is the longest a relay grant lives (exp - iat), minted by the control plane and enforced by the
// relay. A grant is the right to open a NEW relay connection; with the control plane down no fresh one arrives, so a
// short grant ended every relayed path a minute after the outage began, while direct paths and open carriers kept
// working until revoked (PLAN "offline until revoked"). Revocation does not wait for the grant: the relay fences an
// endpoint below a minimum generation (derpquic Server.Revoke) and keeps that fence for this long plus the clock skew.
// Prior art: NetBird's relay token lives 12 h (management/internals/shared/grpc/token_mgr.go); a Tailscale DERP
// server admits a node key with no control token at all.
const MaxGrantLifetime = 12 * time.Hour

// PoolHandoverGrace is how long a relay keeps an account's carriers after a pool change took the relay out of the
// account's pool: longer than the daemons' configuration refresh (about a minute idle), so they have moved to
// the new relays before the old ones close theirs (make-before-break).
const PoolHandoverGrace = 5 * time.Minute

// AccountWireOverhead is how much more than an account's plan speed a relay's account budget lets through. The plan speed is what the
// user sees (payload); the relay counts what it forwards: inner QUIC packet headers and authentication tags, the ACKs flowing back
// through the same budget, and retransmissions of packets it delayed or dropped. Measured at the relay with one datagram per packet, a
// bulk QUIC flow's goodput is 0.92-0.95 of the wire bytes (10.6 Mbit/s through a budget of 11.2; 200 B writes 9.76 of 11.70), so a plan
// speed counted on the wire gave users 5% to 8% less than they bought. The relay cannot see inside the end-to-end encrypted packets, so it
// grosses the budget up instead of counting payload. Prior art: Tailscale's DERP limits the bytes of each frame it forwards
// (derp/derpserver/derpserver.go rateLimit), headers included.
const AccountWireOverhead = 1.06

// AccountStartMbit is what a relay that carries an account but sees none of its traffic is given of the account's speed: enough for a
// new flow's first packets and the account's control traffic, so the flow shows its demand and is given its share on the next
// observation (relayshare.Shares).
const AccountStartMbit = 0.064
