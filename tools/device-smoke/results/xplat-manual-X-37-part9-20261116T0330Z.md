# X-37 part 9 (lane R-X-37G): Unlimited above 4 GiB, Mac to Linux, direct path

Date 2026-11-16 (IST 08:00-08:50). Sender: fleet Mac VM lanex2-m (pbmac, same home LAN as hp), `pb version` dev-a2742604e.
Receiver: `rx37g-l`, a throwaway OS user `rx37g` on hp (Linux x86_64, home LAN Wi-Fi, own user service), enrolled into
the fleet account with `pb machine add`, `pb version` dev-a2742604e. Cleanup: see the end.

## Result: PASS on dev-a2742604e
- `pb ping rx37g-l` from the Mac: first run `path=direct` x4 (0.01-0.07 ms connect); later runs flapped to `regional_relay`
  (12 of 12 samples at 02:29Z) while hp's own `pb doctor` said `peer_paths: lanex2-m direct`.
- Attempt 1 (01:50Z): `pb send big.bin --to rx37g-l --unlimited` printed `Waiting for a direct connection or your relay: only a
  Paperboat relay reaches this machine now`, sent 0 bytes, ended after 22 min with rc=1 "could not finish the command"
  (support_5c5adaa2-446e-46fd-bded-02bdd164a78b). Product gap, see below.
- Attempt 2 (02:29Z, same file): same waiting line, then the send started by itself once the path was direct, 4,718,592,000 B
  delivered, `pb send` rc=0 at 03:14:52Z (45 min including the wait). Receiver Inbox file size 4718592000, sha256
  `5e2ba5c454d2a876c5fbeab4db1d942a37a21bb4b257a66cb983640c252b880f` equal to the sender's `shasum -a 256`.
  No spool copy left (only the `.published` record).

## Found, not fixed
`pb send --unlimited` waits and ends with the generic "could not finish the command" after about 22 min instead of the
documented 32 min wait, and the wait can last 20+ minutes with `pb ping` showing direct. Likely cause (Spark read of
the code): `pb ping` reports each fresh dial's path while the Unlimited gate (`peertransport/tailnet/path_eligibility.go`,
`usage_carrier.go` hostedRelayRecent, 1 s) reads `hosted_relay` whenever any WireGuard data crossed a hosted relay in the last
second, polled by `uploadNative` (`filetransfer/native_client.go`). Not verified on a device.
