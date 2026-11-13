# X-41 part 5: own L + M + W trio, all dev-d77a40713 (main d77a40713, includes 67df20335), 2026-11-13 IST 18:30-22:30, lane R-X-41B

Machines (own, one account, `pb version` of each = dev-d77a40713, commit d77a40713): rx41b-l = Linux container on anvit (Ubuntu 24.04, arm64, own sshd running), rx41b-m = macpool-6 hosted macOS runner (26.7.2 arm64), rx41b-w = winpool-6 hosted Windows runner (runneradmin). Setup problems that were the fixture, not the product: the first Linux containers sat on pbvm2/pbvm3 at load 80-200 (daemon deadlines, every check failed; moved to anvit by the coordinator's order), a container without a running sshd says "Publishing the machine's SSH host keys" until `systemctl enable --now ssh`, the first Windows runner expired mid-run (3 h).

## Removal flavours (harness tools/device-smoke/xplat_removal.py, other two OSes observe; JSON in this folder)
| Flavour | L target | M target | W target |
|---|---|---|---|
| reset | PASS (xplat-removal-20261113T155236Z.json, also T150049Z) | PASS (T155236Z) | PASS (T155236Z, T150049Z) |
| uninstall | PASS (T155236Z) | PASS (T161819Z) | PASS (T155236Z) |
| revoke | PASS (T155236Z) | PASS (T161819Z) | PASS (T162435Z) |
Each PASS = both observers see the old machine gone within 120 s ("No machine has that name or ID", ssh alias removed from their paperboat_config), observers' daemons 8-42 events/min (bound 45), a revoked target says its session is no longer valid (28-38 events/min), the machine enrolled again has a NEW id and identity key and both observers `pb exec` and `pb ssh` it (5-14 s), i.e. no silent host-key change and nothing left that the observers depend on. The new-identity trust is a fresh enrollment: `pb ssh` needed no `trust-host`.

## Tunnels, previews, open flows, shared terminal (tools/device-smoke/xplat_removal_extras.py, revoke of each target; xplat-removal-extras-*.out)
For M, W and L as target: the target's tunnel and preview (live origin) are listed by both observers; one observer opens a terminal session on the target and the other joins it; each observer runs a 900 s `pb exec`. After the observer revokes the target: both terminals end 2-11 s later with a clean message and a support reference (no hang), both execs end rc=255 within 12-20 s ("did not finish ... outcome may be unknown"), and the target's tunnel and preview are gone from both observers' lists. PASS 3/3.

## Failures seen on the way (all in /home/ubuntu/pb-wt/failures.md lines of this lane)
- ONE-WAY INBOUND LOSS TO THE MAC (product bug, open): after the Linux machine was reset/uninstalled and enrolled again, `pb exec` and `pb ping` from L to M timed out for 30+ min while M->L worked; `pb service restart` of L and of M did not help; `launchctl kickstart -k system/com.pinksaucepasta.paperboat.carrier.u501.<gen>` on the Mac cured it at once. Same family as the F21-01/F21-11 M line (rel-outage): the macOS carrier process is the stuck piece. Mac-target flows therefore only passed when run first (xplat-removal-20261113T155236Z.json shows uninstall:M and revoke:M FAIL after the L flows; T161819Z shows them PASS on a fresh run of M targets right after the kickstart). No carrier stack was captured before the kickstart.
- Harness bound: revoke:W failed twice on the 45 events/min bound (55 events on the revoked target, 51 on observer L, the burst right after the revoke; steady state 17-20/min, later runs 36 and 11); not a tight loop, passed on rerun (T162435Z).
- `pb preview N --background` on W without an origin on the port waits 120 s and fails "did not become ready in time" (Linux and Mac return at once with the preview state origin_unavailable); with a live origin on W it is ready. Not a removal issue.

## Not done (cells stay [~])
ENV delivery after removal and config-sync entries after removal: the account of this lane's fixtures (acct-b) already holds an ENV vault of another lane; no stored password unlocks it and a reset would delete that lane's values, a second account (acct-a) console session is expired (401). A fresh account is needed for both parts. Local names, trust wording beyond "fresh enrollment, no trust-host needed" were not exercised separately.
