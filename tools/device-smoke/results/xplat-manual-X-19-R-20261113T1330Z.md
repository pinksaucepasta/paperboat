# X-19 open parts, lane R-X-19, 2026-11-13 (18:30-21:30 IST). Not a tick: the row stays [~]. Fleet churned during the run.

Builds: fleet lanex2-l / lanex2-m dev-79203d6e5, lanex2-w (victus) dev-cf0c0a620 at the start; later the fleet moved to lanex2-l2 (anvit container) / lanex2-w2 (winpool-4) on dev-8cf17c4b0 while hp and victus were down; own Linux container rx19-l on dev-d77a40713.

## VPN state (read-only, recorded 18:35 IST)
- lanex2-w (victus): Tailscale `BackendState Running`, no exit node (`tailscale status`: peers only, no `exit node` line for victus).
- lanex2-m (pbmac VM): `tailscaled` running, `BackendState Running`, `ExitNodeStatus` absent, utun4 100.88.169.42, MagicDNS resolver 100.100.100.100 installed (`scutil --dns` resolver #8).
- So the earlier by-hand fleet pass (results/xplat-manual-X-19-20261113T0430Z.md, names resolve through the public wildcard on every OS) already ran with Tailscale + MagicDNS active on M and W. Not covered: a full-tunnel VPN with an exit node (no machine of ours can switch one on without changing the fleet).

## https://<machine>.<domain>:720 (port below 1024), `xplat_access.py` on fleet, L client
```
L>M https_low_port True (https://lanex2-m.<domain>:720/ answered "xplat-https-ok-low"), forward True, private_address True, https_url True
L>W https_low_port True, forward True, private_address True, https_url True
```
(first six-pair run, results/xplat-access-20261113T130538Z.json; the name check in that run was stale and is fixed in this commit: the public wildcard answers the gateway 127.100.0.1, not the machine's private address). M client rows (M>L, M>W) and W client rows did not give a product result: the fleet record `lanex2-l` was revoked and replaced by `lanex2-l2` during the run (`pb exec lanex2-l` from M: "No machine has that name"), victus went down at ~18:15 IST (ssh timeout) and winpool-4's ssh path dropped afterwards.

## pb doctor with a DNS-rebinding resolver (Linux, own container rx19-l on dev-d77a40713, hp)
Resolver = dnsmasq `--stop-dns-rebind --no-resolv --server=1.1.1.1` in a container; /etc/resolv.conf of rx19-l pointed at it (restored afterwards).
```
getent hosts dns-check.ppr.boats          -> rc=2 (no answer)
pb doctor local_names: warning: Your DNS resolver blocks private answers (DNS rebinding protection), so names under ppr.boats do not resolve here.
  Recovery: Allow ppr.boats in your router or Pi-hole, or use the machine's 127.100.x.y address (`pb resolve <machine>`) with `pb access machine`. ...
baseline (normal resolver): local_names pass on the same machine and build.
```
macOS and Windows not run (no free Mac; victus down, no spare winpool runner).

## Real browser
- Safari on the pbmac VM: the VM's GUI session shows the macOS prompt "Allow "pb" to find devices on local networks?" once the GUI is active (screenshot taken, not committed); Safari showed "Safari can't establish a secure connection" to https://24019.lanex2-l.ppr.boats while that prompt was pending, and the origin saw no request. The target `lanex2-l` was already a stale record then, so this is not proof either way. After accepting the prompt the page still failed against the stale record. Not a pass.
- Edge on victus with Secure DNS (temp profile, Local State dns_over_https mode=secure, headless `--dump-dom`): the browser did not finish within 60 s over the ssh session (no DOM, empty net log); victus was then unreachable. Not a pass.

## Team grant/setting revoke ending a held flow
Delegated harness (branch rx19t-team-private-end, 876bc4620, not merged): 0 of 6 pairs complete (hp down; fixture containers lost). Only W>L isolated runs 6.99 s / 17.55 s for the grant revoke, excluded. Not a pass.

## Sweep failure "access tcp name-resolve" (SWEEP1) = the check, fixed; L<->M rerun passes (22:10 IST)
The check compared `gethostbyname(<machine>.<domain>)` with the machine's private address; the public wildcard answers the gateway 127.100.0.1, and the printed message was the tail of pb's usage text after the refusal. Fixed (commit aa6e69db9, pushed 9cdace229). Rerun on fleet lanex2-l2 and lanex2-m (`pb version` both dev-8cf17c4b0), results/xplat-access-20261113T153856Z.json, run under fleet-mac.lock:
```
PASS X-19:tcp:L>M forward and 127.100.0.4 byte-exact with half-close; non-loopback --listen refused
PASS X-19:tcp:M>L forward and 127.100.0.34 byte-exact with half-close; non-loopback --listen refused
```
In non-core mode each pair also asserts: name resolves to 127.100.0.1, `https://<port>.<machine>.<domain>` and `https://<machine>.<domain>:720` (port below 1024, explicit `pb tunnel policy allow`) answer through the OS trust store. So the M client low-port failure seen earlier was the revoked `lanex2-l` record, not a product result.
Not rerun: any pair with the Windows runner (winpool-4 ssh dropped, victus down).
