# REL-EDGE Linux first pass

Tested committed build: `261b23966` (2026-11-12 UTC). Linux runners; Go 1.27.4.
No device binaries installed; no deployment.

All executed tests below passed (remote-test exit 0). First shared and CLI transfers to a second runner exited 255 before testing; rerun on the working runner passed.

Live staging observations NOT RUN: build host load 51.49 at 21:43 UTC and 44.91 at 21:47 UTC exceeds required <16. Read-only staging status reports server commit `8a8defaa60bf4961e25918105cbf11e6a289191a`; current edge build not verified. No staging changes made.

| Row | First-pass outcome / gap |
|---|---|
| F21-17 | PASS outage/cache/accept tests; staging outage-serving observation blocked by load. |
| F21-22 | Ready leases use push and pending backoff test passes, but pending still polls (250 ms rising to 2 s); explicit pending-push requirement unmet; requires CLI hostruntime owner. Deleted connector heartbeat path has no applicable tunnel/shared test. |
| F21-28 | PASS real QUIC listener, indexed-rule test and benchmark; edge blackhole liveness untested. Edge config still has 2 min idle / 30 s default keepalive (client advertises 15 s / 5 s, negotiated timeout is lower of peers). Live check blocked by load. |
| F21-36 | PASS identity index and change notification; lookup 564.3 ns/op at 10 admissions, 579.4 at 4,096. Staging F21-17 observation blocked by load. |
| F21-37 | PASS one-hour default, public/private idle-close and separate accept-slot tests; staging idle-stream observation blocked by load. |
| F21-38 | PASS endpoint delta, route version and ingress idle-byte tests. Endpoint worker: 1 full + 21 deltas, new route installed in 40.08 ms. Real idle-edge request/byte budget blocked by load. |
| F21-47 | PASS shared default validity, heartbeat answer and self-hosted fallback tests; deployed limits/config equality on both edges not observed (load). |

## Automated check transcript (trimmed)

Run 1:

```text
go version go1.27.4 linux/amd64
=== RUN   TestHeartbeatAnswerCarriesIngressLimits
--- PASS: TestHeartbeatAnswerCarriesIngressLimits (0.00s)
=== RUN   TestHTTPClientRouteSnapshotAsksForAVersion
--- PASS: TestHTTPClientRouteSnapshotAsksForAVersion (0.00s)
=== RUN   TestIngressAuthoritySelectsExactMatchedAlias
--- PASS: TestIngressAuthoritySelectsExactMatchedAlias (0.00s)
=== RUN   TestIngressAuthorityInvalidationRefreshesNewRouteAndFencesInFlightSnapshot
--- PASS: TestIngressAuthorityInvalidationRefreshesNewRouteAndFencesInFlightSnapshot (0.01s)
=== RUN   TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay
=== RUN   TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay/ready
=== RUN   TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay/denied
--- PASS: TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay (0.02s)
    --- PASS: TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay/ready (0.01s)
    --- PASS: TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay/denied (0.01s)
=== RUN   TestIngressAuthorityRefreshesDecisionBeforeCacheInterval
=== RUN   TestIngressAuthorityRefreshesDecisionBeforeCacheInterval/renewed
=== RUN   TestIngressAuthorityRefreshesDecisionBeforeCacheInterval/withdrawn
=== RUN   TestIngressAuthorityRefreshesDecisionBeforeCacheInterval/unavailable
--- PASS: TestIngressAuthorityRefreshesDecisionBeforeCacheInterval (0.25s)
    --- PASS: TestIngressAuthorityRefreshesDecisionBeforeCacheInterval/renewed (0.09s)
    --- PASS: TestIngressAuthorityRefreshesDecisionBeforeCacheInterval/withdrawn (0.08s)
    --- PASS: TestIngressAuthorityRefreshesDecisionBeforeCacheInterval/unavailable (0.08s)
=== RUN   TestIngressAuthorityKeepsPublicDecisionsThroughAControlOutage
--- PASS: TestIngressAuthorityKeepsPublicDecisionsThroughAControlOutage (0.20s)
=== RUN   TestIngressAuthorityAsksForAVersionAndIdleBytesStaySmall
--- PASS: TestIngressAuthorityAsksForAVersionAndIdleBytesStaySmall (0.07s)
=== RUN   TestIngressAuthorityAcceptNeverWaitsOnAHangingControlPlane
--- PASS: TestIngressAuthorityAcceptNeverWaitsOnAHangingControlPlane (1.02s)
=== RUN   TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace
=== RUN   TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace/in_grace
=== RUN   TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace/no_takeover
--- PASS: TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace (2.59s)
    --- PASS: TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace/in_grace (1.07s)
    --- PASS: TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace/no_takeover (1.52s)
=== RUN   TestIngressAuthorityPreviewMissesInGraceShareFetchesAndBackOff
--- PASS: TestIngressAuthorityPreviewMissesInGraceShareFetchesAndBackOff (1.22s)
=== RUN   TestIngressAuthorityTakesAnAnswerSlowerThanTheControlTimeout
--- PASS: TestIngressAuthorityTakesAnAnswerSlowerThanTheControlTimeout (1.51s)
=== RUN   TestIngressAuthorityIsWarmOnlyAfterTheFirstSuccessfulFetch
--- PASS: TestIngressAuthorityIsWarmOnlyAfterTheFirstSuccessfulFetch (0.00s)
=== RUN   TestIngressAuthoritySharedFetchOutlivesTheCallerThatStartedIt
--- PASS: TestIngressAuthoritySharedFetchOutlivesTheCallerThatStartedIt (0.61s)
=== RUN   TestIngressAuthorityRefusesQuotaDeniedRouteWithTypedReasonAndRecovers
--- PASS: TestIngressAuthorityRefusesQuotaDeniedRouteWithTypedReasonAndRecovers (0.00s)
=== RUN   TestIngressAuthorityRefusesQuotaDeniedPreviewWithTypedReason
--- PASS: TestIngressAuthorityRefusesQuotaDeniedPreviewWithTypedReason (0.00s)
=== RUN   TestIngressAuthorityRefusesBlockedHostname
--- PASS: TestIngressAuthorityRefusesBlockedHostname (0.00s)
=== RUN   TestIngressAuthorityAppliesOwnerIPRules
--- PASS: TestIngressAuthorityAppliesOwnerIPRules (0.00s)
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/control	7.534s
=== RUN   TestExpectedAdmissionIndexByCarrierIdentity
--- PASS: TestExpectedAdmissionIndexByCarrierIdentity (0.00s)
=== RUN   TestExpectsNamesOnlyUnexpiredAdmittedHosts
--- PASS: TestExpectsNamesOnlyUnexpiredAdmittedHosts (0.00s)
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier	0.006s
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/route	0.003s [no tests to run]
=== RUN   TestPublicTCPIdleTimeoutDefaultsToOneHour
--- PASS: TestPublicTCPIdleTimeoutDefaultsToOneHour (0.00s)
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/config	0.005s
=== RUN   TestServerIngressLimitsApplyToCurrentKeys
--- PASS: TestServerIngressLimitsApplyToCurrentKeys (0.00s)
=== RUN   TestEdgeWithoutAnswerUsesTheSharedDefaultLimits
--- PASS: TestEdgeWithoutAnswerUsesTheSharedDefaultLimits (0.00s)
=== RUN   TestPrivateTCPStreamClosesWhenIdle
--- PASS: TestPrivateTCPStreamClosesWhenIdle (0.75s)
=== RUN   TestPublicTCPConnectionsGetKeepAlive
--- PASS: TestPublicTCPConnectionsGetKeepAlive (0.00s)
=== RUN   TestPublicTCPListenersBindBeforeReadyObservation
--- PASS: TestPublicTCPListenersBindBeforeReadyObservation (0.00s)
=== RUN   TestPublicTCPListenersBindPersistAndWithdrawExactReservation
--- PASS: TestPublicTCPListenersBindPersistAndWithdrawExactReservation (0.00s)
=== RUN   TestPublicTCPFatalAcceptWithdrawsOnlyItsIdentityAndRebinds
--- PASS: TestPublicTCPFatalAcceptWithdrawsOnlyItsIdentityAndRebinds (0.00s)
=== RUN   TestPublicTCPObservationKeepsMixedOperationalCause
--- PASS: TestPublicTCPObservationKeepsMixedOperationalCause (0.00s)
=== RUN   TestPublicTCPListenersRefusePortsOutsideTheRange
--- PASS: TestPublicTCPListenersRefusePortsOutsideTheRange (0.00s)
=== RUN   TestForwardPublicTCPValidatesAuthorityAndPreservesHalfClose
--- PASS: TestForwardPublicTCPValidatesAuthorityAndPreservesHalfClose (0.00s)
=== RUN   TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier
=== RUN   TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier/wrong_port
=== RUN   TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier/wrong_target
=== RUN   TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier/expired
--- PASS: TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier (0.05s)
    --- PASS: TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier/wrong_port (0.00s)
    --- PASS: TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier/wrong_target (0.00s)
    --- PASS: TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier/expired (0.00s)
=== RUN   TestPublicTCPAdmittedStreamOutlivesTTLAndIsCutByRevocation
--- PASS: TestPublicTCPAdmittedStreamOutlivesTTLAndIsCutByRevocation (1.00s)
=== RUN   TestPublicTCPIdleTimeoutClosesQuietStreamsCleanly
--- PASS: TestPublicTCPIdleTimeoutClosesQuietStreamsCleanly (1.06s)
=== RUN   TestForwardPublicTCPOnAnEngineCarrierSendsNoDecisionPreface
--- PASS: TestForwardPublicTCPOnAnEngineCarrierSendsNoDecisionPreface (0.00s)
=== RUN   TestTLSPassthroughStreamReleasesItsAcceptSlot
--- PASS: TestTLSPassthroughStreamReleasesItsAcceptSlot (0.51s)
=== RUN   TestSharedTLSHTTPSConnectionsAreCapped
--- PASS: TestSharedTLSHTTPSConnectionsAreCapped (0.51s)
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp	3.884s
```

Run 2:

```text
go version go1.27.4 linux/amd64
=== RUN   TestEndpointPullTakesDeltasBetweenFullSets
    endpoint_delta_test.go:46: full endpoint answer, 10,000 endpoints on 1 carriers: 1712360 bytes in 20 pages, largest page 85618 bytes
    endpoint_delta_test.go:46: full endpoint answer, 10,000 endpoints on 1000 carriers: 3211760 bytes in 20 pages, largest page 160588 bytes
    endpoint_delta_test.go:46: full endpoint answer, 10,000 endpoints on 10000 carriers: 16981760 bytes in 20 pages, largest page 849088 bytes
    endpoint_delta_test.go:110: 1 full sets and 21 deltas; a new route installed after 40.077924ms
--- PASS: TestEndpointPullTakesDeltasBetweenFullSets (0.26s)
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/runtime	0.270s
=== RUN   TestQUICListenerEdgeOpenConnectorAcceptAndALPN
--- PASS: TestQUICListenerEdgeOpenConnectorAcceptAndALPN (0.01s)
BenchmarkForIdentityWithManyAdmissions
BenchmarkForIdentityWithManyAdmissions/admissions=10
BenchmarkForIdentityWithManyAdmissions/admissions=10-4         	  454546	       564.3 ns/op
BenchmarkForIdentityWithManyAdmissions/admissions=4096
BenchmarkForIdentityWithManyAdmissions/admissions=4096-4       	  356610	       579.4 ns/op
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier	0.715s
=== RUN   TestRuleIndexChoosesLikeTheFullScan
--- PASS: TestRuleIndexChoosesLikeTheFullScan (0.00s)
BenchmarkAcquireWithManyRules
BenchmarkAcquireWithManyRules/10
BenchmarkAcquireWithManyRules/10-4         	  103851	      2032 ns/op
BenchmarkAcquireWithManyRules/4096
BenchmarkAcquireWithManyRules/4096-4       	  104688	      2282 ns/op
PASS
ok  	github.com/pinksaucepasta/paperboat/tunnel/internal/route	2.534s
```

Run 3:

```text
go version go1.27.4 linux/amd64
=== RUN   TestDefaultLimitsAreValidAndKeepOneStreamAbove150Mbps
--- PASS: TestDefaultLimitsAreValidAndKeepOneStreamAbove150Mbps (0.00s)
PASS
ok  	github.com/pinksaucepasta/paperboat/shared/edgelimits	0.002s
=== RUN   TestIngressVersionIgnoresPerAnswerValuesAndOrder
--- PASS: TestIngressVersionIgnoresPerAnswerValuesAndOrder (0.00s)
PASS
ok  	github.com/pinksaucepasta/paperboat/shared/connectorprotocol	0.003s
```

Run 4:

```text
go version go1.27.4 linux/amd64
=== RUN   TestLeaseObserverCarrierReadsReadyLeaseOnlyOnChange
--- PASS: TestLeaseObserverCarrierReadsReadyLeaseOnlyOnChange (0.50s)
=== RUN   TestLeaseObserverPendingPollBacksOff
--- PASS: TestLeaseObserverPendingPollBacksOff (0.00s)
PASS
ok  	github.com/pinksaucepasta/paperboat/internal/hostruntime/preview	0.508s
```
