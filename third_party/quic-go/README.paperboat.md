# quic-go, patched for Paperboat

Upstream is github.com/quic-go/quic-go v0.61.0. The vendored subset keeps the upstream license and the congestion-control tests.
The CLI, relay and edge modules select this copy with a Go module replacement.

## PAPERBOAT PATCH (RFC 9406)

HyStart++ compares the minimum RTT of the current round with the preceding round. Persistent delay enters Conservative Slow
Start, which grows the congestion window by one quarter of newly acknowledged bytes. A sufficiently sampled round below
the entry baseline resumes regular slow start. Five CSS rounds, including the entry round, end startup. The existing CUBIC
loss and ECN response can end either phase immediately.

The ACK handler supplies one fresh, ACK-delay-adjusted RTT sample per eligible 1-RTT ACK frame, after applying window
growth and loss or ECN processing. Tracking rounds in the application packet number space prevents handshake packet
numbers from advancing them. The sender is paced and uses the RFC's uncapped acknowledged-byte growth. Its existing
application-limited guard still applies.

When updating the vendored version, look for `PAPERBOAT PATCH (RFC 9406)` and run the congestion tests and relay
long-path regression. The implementation follows RFC 9406, sections 4.2 and 4.3.
For comparison, FreeBSD's NewReno implementation tracks the two round minima and the CSS entry baseline in
`sys/netinet/cc/cc_newreno.c`.

## PAPERBOAT PATCH (BBR)

`quic.Config.CongestionControl` picks the sender per connection: `CongestionControlReno` (the zero value, NewReno with
HyStart++), `CongestionControlCubic` (CUBIC with HyStart++) or `CongestionControlBBR`. Each end picks its own. Paperboat's
QUIC sites set BBR: the peer QUIC session, the relay carrier and the edge data carrier.

BBRv1 is Hysteria's sender (`internal/congestion/bbr`, MIT; provenance and licenses in its `NOTICE.md`). BBR needs each ACK's
acknowledged and lost packets together, so the ACK handler fills `ackedPacketsInfo` and `lostPacketsInfo` and calls
`OnCongestionEventEx` once per ACK frame (and once per loss-timer expiry). Reno and CUBIC ignore it.

When updating the vendored version, grep `PAPERBOAT PATCH (BBR)`: `config.go`, `interface.go`, `connection.go` (two
`NewSentPacketHandler` calls), `internal/ackhandler/sent_packet_handler.go` (constructor, `MigratedPath`, `ReceivedAck`,
`detectLostPackets`, `OnLossDetectionTimeout`), `internal/congestion/interface.go`. Then run `internal/congestion/...`
tests (the simulation in `bbr/sim_test.go` covers loss and a policer) and the relay benchmark.
