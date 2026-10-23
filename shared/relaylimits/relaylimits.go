// Package relaylimits holds the bounds that the control plane and the relay both enforce on a
// relay node, so the registry's per-node capacity and the relay's own ceiling cannot disagree.
package relaylimits

// MaxConnectionsPerNode is the largest capacity_limit a relay node may be registered with: the
// validation ceiling, not a node's size. An operator sets each node's real capacity in the
// registry (capacity_limit) from its memory and file descriptors. A relay connection costs at
// most ~256 KiB of buffers (usually far less) and one file descriptor, so 16384 connections
// fit in ~4 GiB at the worst case.
const MaxConnectionsPerNode = 16384
