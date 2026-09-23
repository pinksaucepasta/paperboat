// Copyright (c) Paperboat contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package mesh assembles Tailscale's userspace WireGuard, netstack and magicsock
// engine for Paperboat's authorized virtual UDP transport. It is adapted from
// Tailcat commit 5a83b9f9e119aad6b558cbc122d94efdca87452d; upstream attribution and
// notices are retained in upstream/licenses/tailcat-LICENSE.txt.
//
// All endpoints use one symmetric Server engine with an explicit node key,
// allocated IPv6 address and replaceable peer policy. No implicit identity,
// public relay lookup, TCP service, host forwarding or Tailscale control plane
// is provided. Paperboat's signed authority and operation grants live above this
// package. Discovery messages and descriptor encoding retain their pinned wire
// semantics; renaming the Go package must not rotate discovery identities.
package mesh
