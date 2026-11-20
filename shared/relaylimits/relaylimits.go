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
