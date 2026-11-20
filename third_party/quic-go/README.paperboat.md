# quic-go, patched for Paperboat

Upstream: github.com/quic-go/quic-go **v0.61.0**, copied unchanged except as listed here. Tests, examples, fuzzing and
integration-test directories were left out; `internal/congestion` keeps its tests. LICENSE is upstream's.
Used through `replace github.com/quic-go/quic-go => ../third_party/quic-go` in `cli/`, `relay/` and `edge/`.

## Patch (one change, `internal/congestion/hybrid_slow_start.go`, marked `PAPERBOAT PATCH`)

HyStart (the delay check that ends slow start) used `threshold = clamp(minRTT/8, 4 ms, 16 ms)`. The patch drops the 16 ms
upper clamp: `threshold = max(minRTT/8, 4 ms)`.

Why: on a 500 ms path (US peers through an India relay) about 30 ms of delay jitter is normal. With the clamp at 16 ms, slow
start ended after ~1.1 s at ~69 KB of congestion window with no loss, and congestion avoidance then added about one packet per
round trip: ~1.4 Mbit/s whatever the plan, for any QUIC flow on a long or jittery path (direct, relay, carrier). With the
patch a 5 Mbit/s account gets ~4.2 Mbit/s at 500 ms; paths at 250, 100 and 4 ms are unchanged (measured, see below).

Tests: `internal/congestion` `TestHybridSlowStartLongPathIgnoresJitterBelowAnEighth`;
`relay/derpquic` `TestQUICBulkOverALongPathGetsMostOfTheAccountSpeed` (fails 1.4 Mbit/s without the patch).

## Updating

Replace the directory with the new upstream version, re-apply the patch (grep `PAPERBOAT PATCH`), re-run both tests. Drop the
patch if upstream makes the threshold configurable or removes the clamp (issue draft in paperboat-private/relay-500ms.md).
