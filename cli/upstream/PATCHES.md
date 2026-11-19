# Paperboat networking provenance and patches

The owned assembly in `internal/peertransport/mesh` is adapted from Tailcat
`5a83b9f9e119aad6b558cbc122d94efdca87452d`. Its BSD-3-Clause copyright headers
and `upstream/licenses/tailcat-LICENSE.txt` are retained. The original compatible
source/module/toolchain facts remain in `foundations.tsv`; Tailcat is no longer
a consumed module. `references/tailcat` remains read-only provenance.

The consumed fork revision is `b2eaa82185fc` on `win-status-trust-1.104` (`paperboat-1.104` `be55aaa472c8` plus the status patch below) in
`github.com/pinksaucepasta/tailscale`, based on upstream Tailscale **v1.104.7**
(`cbc74fe48`). Both Paperboat and the relay pin its immutable Go module version.
Its LICENSE, PATENTS and other notices remain intact. `foundations.tsv` retains the
original Tailcat-compatible reference pair as provenance; no upstream application
is built for this update.

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

Twelve commits on upstream v1.104.7. Each is removed when its removal condition holds.

| Files | Patch and owner | Removal condition |
| --- | --- | --- |
| `wgengine/magicsock/{magicsock,derp,paperboat_regional_status}.go`, `wgengine/userspace.go` | Injectable DERP carrier (`DERPCarrierFactory`, `DERPControlSender`), reliable discovery/bootstrap send (`SendDERPPacketTo` as control, `SendDERPPacketToRegion` to the exact region), fatal receive-error classification with late-reader fencing, actual carrier leg (`RelayTransport`), liveness probe on socket rebind fenced per carrier (`derp_terminal_error_test.go`, `rebind_carrier_test.go`). Packet-body debug logging removed. | Reviewed upstream equivalent carrier API and passing authority/recovery regressions. |
| `wgengine/userspace.go` | Thread magicsock `TestOnlyPacketListener` through the engine for real path/failure fixtures. | Equivalent upstream test seam. |
| `net/udprelay/server.go` | Trusted allocation policy (`AllocateEndpointWithPolicy`, `SetEndpointAuthorizer`, `SetForwardingLimiter`, `RevokeEndpoint`): absolute expiry/capacity, per-packet authorization, forwarding quota, packet bounds, zero client discovery-key guard in the shared allocator (`paperboat_policy_test.go`). | Reviewed upstream equivalent scoped lifecycle API. |
| `wgengine/magicsock/endpoint.go` | Clear the relay-discovery throttle on connectivity change so a rebind re-discovers immediately (`paperboat_rebind_test.go`). | Reviewed upstream equivalent recovery behavior. |
| `wgengine/magicsock/{derp,magicsock}.go` | Magicsock never picks a STUN-only region as DERP home or fallback (`pickDERPFallback`, `maybeSetNearestDERP`, `setNearestDERP`; `stun_only_test.go`); per-family `localEndpointCandidates` advertises each family's own bound port (`local_endpoints_test.go`). | Upstream magicsock filters STUN-only regions and uses per-family ports. |
| `net/netcheck/netcheck.go`, `tailcfg/derpmap.go` | Backport of upstream `dc6986976` (`DERPRegion.CanServeDERP`, STUN-only never the netcheck home). It is not in v1.104.7. | Drop on the first stable release containing `dc6986976`. |
| `wgengine/magicsock/paperboat_underlay_trace.go` + hooks | Bounded underlay metadata (direction, type, size, endpoint, failure, transport; no bodies or keys) through the existing logger. Parsed by `mesh/underlay_diagnostics.go` and `cmd/pb/reconnect_metadata.go`. | Upstream per-packet metadata hook, or those parsers removed. |
| `wgengine/magicsock/endpoint.go` | Lossy direct-path series: 10-probe heartbeat window per address (`endpoint.pathLoss`, bounded, cleared on network change); 3 lost heartbeats demote the direct path to relay with a doubling hold-down and probation; only a heartbeat timeout counts as loss. Fixes 10-30x throughput collapse on 16-61% loss paths. | Upstream path-quality demotion. |
| `wgengine/magicsock/endpoint.go` | `populatePeerStatus` reports `CurAddr`/`PeerRelay` only while `trustBestAddrUntil` is open (or a WireGuard-only endpoint). Upstream sets them whenever `addrForSendLocked` returns a UDP address without a DERP one, which after the trust window also holds for a peer with no DERP address: an expired, silent `bestAddr` stayed the current direct path in status (`pb status` showed `Path: direct` for 30-42 s into a cut, victus 2026-11-19). `Test_endpoint_populatePeerStatusReportsDirectOnlyWhileTrusted` (fails without the patch). **Upstream candidate**: the same bug is in Tailscale's `populatePeerStatus`; send it upstream. | Upstream fixes `populatePeerStatus` to require a trusted address. |
| `util/cloudenv/cloudenv.go` | No metadata-server probe for old GCP, Azure or Hyper-V guests: Paperboat never uses the cloud identity and the probe cost up to 2 s on every Hyper-V cold start. | Never needed upstream; remove if Paperboat starts using cloud identity. |
| `wgengine/magicsock/magicsock_test.go` | `TestTwoDevicePing` accepts the IPv6 loopback path; `TestConn_receiveIP` sets `logf` (needed by the underlay trace). Test-only. | Underlay trace removed and the IPv6 flake gone upstream. |

Dropped at the 1.104.7 rebase: the fork's own `HasDERP` helper and its stricter
netcheck `prevDERP`/forced-DERP delta (the upstream backport above replaces them;
Paperboat never sets `ForcePreferredDERP`). Paperboat supplies supplementary
discovery separately through signed regional configuration; these destinations
never receive application traffic or relay credentials.

WireGuard and gVisor come from the v1.104.7 `go.mod`; Paperboat's QUIC transport
uses WireGuard's UDP path, so Tailcat's gVisor TCP benchmark is not evidence of a
Paperboat performance gain. Tailcat 0.7 peer path status informed the authorized,
address-free `mesh.Server.PeerPath` diagnostic; its Windows localhost fix informed
Paperboat's private-preview IPv6 loopback fallback. Tailcat's standalone
Listen/exit-node/SSH/SCP/browser/Android/packaging modes are not consumed.
