# Paperboat networking provenance and patches

The owned assembly in `internal/peertransport/mesh` is adapted from Tailcat
`5a83b9f9e119aad6b558cbc122d94efdca87452d`. Its BSD-3-Clause copyright headers
and `upstream/licenses/tailcat-LICENSE.txt` are retained. The original compatible
source/module/toolchain facts remain in `foundations.tsv`; Tailcat is no longer
a consumed module. `references/tailcat` remains read-only provenance.

The consumed Tailscale snapshot remains based on
`31d8badb3bfb88618dc8ea8e6a5c3bce0cd6cc9f`, through the existing `go.mod`
replacement. Its LICENSE, PATENTS and other notices remain intact. No fork or
release has been published; no upstream applications need building.

## Owned Tailcat-derived assembly

The limited extraction replaces `tailcat.go` with `mesh/{engine,packet,descriptor,keys}.go`
and retains `disco.go`, `wire.go`, `regional.go`, `regional_status.go` and applicable
tests. Standalone Client, SSH/SFTP/TCP/host forwarding, implicit identities,
key-derived addresses, public relay lookup/cache, deprecated aliases and application
build/demo machinery are removed. Discovery derivation and descriptor wire semantics
remain pinned; these are not branding strings to rename.

Previously downstream Tailcat patches remain owned behavior:

| Origin/owner | Preserved behavior and evidence |
| --- | --- |
| Task 6 | No fallback host UDP forwarding for late replies to retired client ports. Outbound-only engines reject new inbound flows. Real QUIC restart and goroutine-leak regression retained on signed-authority fixtures. |
| Task 7 | Explicit allocated IPv6 addresses, empty-deny peer maps, exact source/UDP-port filtering, live WireGuard removal through `SyncDevicePeer`. Adapted from Tailscale `wgengine/userspace.go` and magicsock peer removal/rotation (`TestPeerDERPStateCleanup`). Authority tests retained. |
| Task 10 | Discovery-only peer relay nodes, RelayTarget capability installed in both TUN and magicsock filters, immutable metadata/capability versions, test socket injection. Application authority stays above mesh. |
| Task 11 | Endpoint-owned `DERPCarrierFactory` passed into wgengine. |
| Task 13 | Live signed regional inventories, exact-node control/preparation, peer promotion after pair proof, actual carrier status and promoted bootstrap region. Authority and regional tests retained. |
| Task 23.5 | Direct-only authority startup with an empty relay map, without public relay discovery. Existing `tailnet/relay_empty_test.go` retained. |
| Limited extraction, 2026-09-19 | Authority-only symmetric engine, no TCP or unspecified-port admission, zero discovery-key rejection at bootstrap, explicit identity and self-key validation; source/descriptor tests retained. Bootstrap retries follow the currently promoted peer region instead of retaining an unreachable provisional relay; regional failover regression retained. Default engine logging is silent and descriptor parse errors do not echo preshared-key input. `tailnet` packet I/O additionally checks an atomic authority-expiry fence independently of timer cleanup. |

## Maintained Tailscale patches

| Files | Patch and owner | Removal condition |
| --- | --- | --- |
| `wgengine/userspace.go`, `wgengine/magicsock/{magicsock,derp}.go` | Injectable region carrier, reliable discovery/bootstrap send hook, fatal receive-error classification; preserve exact fatal authority errors after reader cleanup and fence late replacement readers (`derp_terminal_error_test.go`, Tasks 11/23). Packet-body debug logging removed. | Reviewed upstream equivalent carrier API and passing authority/recovery regressions. |
| `net/udprelay/server.go` | Trusted allocation policy with absolute expiry/capacity, immediate per-packet authorization, post-source-validation forwarding quota, packet bounds, safe endpoint revocation and remaining-authority lifetimes. Existing Geneve/disco protocol retained (`paperboat_policy_test.go`, Task 10). | Reviewed upstream equivalent scoped lifecycle API. |
| `wgengine/userspace.go` | Thread magicsock packet-listener test injection through the engine for real path/failure fixtures (Task 10). | Equivalent upstream test seam. |
| `wgengine/magicsock/endpoint.go` | Clear relay-discovery throttle with stale selected route on connectivity changes, allowing immediate rebind (`paperboat_rebind_test.go`, Task 10). | Reviewed upstream equivalent recovery behavior. |
| `wgengine/magicsock/{derp.go,paperboat_regional_status.go}` | Preserve live inventory/carrier state, prepare/send through exact region, expose actual carrier legs; injected carriers probe liveness on socket rebind, with stale probes fenced from newer carriers (Task 13). | Equivalent upstream regional/carrier seams. |
| `wgengine/magicsock/relaymanager.go`, `net/udprelay/server.go` | Port upstream security fix `2a4ce5ba3b415331438ad5db1c18b2e916159e4d`: reject zero relay/server or client disco keys before shared-secret derivation. Tests `TestRelayManagerZeroServerDisco` and `TestAllocateEndpointZeroClientDisco` retained. The allocator guard is placed in the shared allocator to protect both ordinary and policy APIs (2026-09-19). | Consumed pin includes this fix and both regressions pass. |

Current upstream review: Tailcat main
`15ab9e68bfc6534a61797d7af28cedd42b54a3a5` has newer Tailscale/WireGuard/gVisor
pins, but does not replace our authority and regional API patches. Only the focused
zero-key security fix above is ported; module versions are not silently advanced.
