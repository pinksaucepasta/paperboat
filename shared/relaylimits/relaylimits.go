// Package relaylimits holds the bounds that the control plane and the relay both enforce on a
// relay node, so the registry's per-node capacity and the relay's own ceiling cannot disagree.
package relaylimits

import "time"

// MaxConnectionsPerNode is the largest capacity_limit a relay node may be registered with: the
// validation ceiling, not a node's size. An operator sets each node's real capacity in the
// registry (capacity_limit) from its memory and file descriptors. A relay connection costs at
// most ~256 KiB of buffers (usually far less) and one file descriptor, so 16384 connections
// fit in ~4 GiB at the worst case.
const MaxConnectionsPerNode = 16384

// MaxGrantLifetime is the longest a relay grant lives (exp - iat), minted by the control plane and enforced by the
// relay. A grant is the right to open a NEW relay connection; with the control plane down no fresh one arrives, so a
// short grant ended every relayed path a minute after the outage began, while direct paths and open carriers kept
// working until revoked (PLAN "offline until revoked"). Revocation does not wait for the grant: the relay fences an
// endpoint below a minimum generation (derpquic Server.Revoke) and keeps that fence for this long plus the clock skew.
// Prior art: NetBird's relay token lives 12 h (management/internals/shared/grpc/token_mgr.go); a Tailscale DERP
// server admits a node key with no control token at all.
const MaxGrantLifetime = 12 * time.Hour
