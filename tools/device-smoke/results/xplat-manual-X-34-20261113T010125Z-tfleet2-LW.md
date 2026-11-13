# T-FLEET2: X-34 L<->W (personal workspace of the fleet account) on main dev-94eb504aa, 2026-11-13 06:25-06:32 IST

Machines lanex2-l (Linux) and lanex2-w (Windows), `pb version` dev-94eb504aa on both. The Mac was offline (another lane's staging install), so this is the L<->W pair only.
Fixtures: the personal workspace policy had version 17 (digest sha256:4cd372b3...), restored at the end (version 23, same digest). The policy file was written on W by PowerShell `Out-File -Encoding Unicode` (UTF-16 LE BOM, CRLF lines; first bytes 255,254,123,0). The owner's address is shown as <owner>.
Policy file: a tag `tag:tf2` on both machines, `group:tf2` (the owner), a rule allowing terminal, exec, managed_ssh, files, preview_manage, tunnel_manage on the tag, a deny rule for exec on the Windows machine, and 2 in-file tests.
Not in a personal workspace (team only): private_access ports, `env:team`, member refusals; those need the team fixture (see failures.md, X-34 L member line).

## versions: L dev-94eb504aa W dev-94eb504aa
baseline policy version 17 sha256:4cd372b363e5493a179e2d0b20deb5c6510ac9c7e74c646acc493271fbec1d83
W wrote UTF-16 file: size 1600 first bytes 255,254,123,0 crlf 12
## A: file checks, each client
L policy test rc 0 Tests: 2 passed, 0 failed / No access changes.
L policy set --dry-run rc 0 Tests: 2 passed, 0 failed / No access changes.
W policy test rc 0 Tests: 2 passed, 0 failed / No access changes.
W policy set --dry-run rc 0 Tests: 2 passed, 0 failed / No access changes.
W policy set --yes (UTF-16 BOM CRLF) rc 0 Tests: 2 passed, 0 failed / No access changes. / Saved policy version 18 (sha256:ee95eadd78a59e04b60734ae057ffd789495d7b9ad44b18097386a94275783b1).
policy get after W save: L version 18 W version 18 digest equal True text has tag:tf2 True comment kept True
## B: explain / test text, L vs W
explain exec machine:lanex2-l | L: 0 ALLOWED: owner | W: 0 identical text: True
explain exec machine:lanex2-w | L: 0 ALLOWED: owner | W: 0 identical text: True
explain files machine:lanex2-w | L: 0 ALLOWED: owner | W: 0 identical text: True
explain terminal machine:lanex2-l | L: 0 ALLOWED: owner | W: 0 identical text: True
explain managed_ssh machine:lanex2-l | L: 0 ALLOWED: owner | W: 0 identical text: True
explain preview_manage machine:lanex2-l | L: 0 ALLOWED: owner | W: 0 identical text: True
explain tunnel_manage machine:lanex2-w | L: 0 ALLOWED: owner | W: 0 identical text: True
policy test (saved policy) L (0, 'Tests: 2 passed, 0 failed\nNo access changes.') W 0 identical True
who can exec lanex2-w: L Who can (1): / SUBJECT                            TARGET            ACTION  PORTS  REASON  RULES / user:<owner>  machine:lanex2-w  exec    -      owner   - | identical True
refusal text for an invalid policy: L (2, '/rules/0/dst/0: unknown_machine: machine "nope" is not in this workspace\nTests: 0 passed, 0 failed\nNo access changes.\npb: the policy is not valid: 1 error(s)\nUsage:\n  pb policy set <file|-> [flags]\n\nE') | W 2 | same text True
after the refused saves, version unchanged on both: 18 18
## C: tags from the other OS
W pb machine tag lanex2-l add tf2b rc 0 Tags of lanex2-l: tag:tf2, tag:tf2b (policy version 19).
  seen from the other OS: tag list Tags of lanex2-l: tag:tf2, tag:tf2b. | machine list --tag tf2b ['lanex2-l']
  removed rc 0
L pb machine tag lanex2-w add tf2c rc 0 Tags of lanex2-w: tag:tf2, tag:tf2c (policy version 21).
  seen from the other OS: tag list Tags of lanex2-w: tag:tf2, tag:tf2c. | machine list --tag tf2c ['lanex2-w']
  removed rc 0
## C2: API-token-scoped explain (a non-owner principal), L vs W
create scoped token rc 0
token explain exec machine:lanex2-l | L: 0 ALLOWED: owner | W: 0 identical True
token explain exec machine:lanex2-w | L: 0 ALLOWED: owner | W: 0 identical True
token explain files machine:lanex2-l | L: 0 DENIED: outside_token_scope | W: 0 identical True
revoke tf2-x34 rc 0 Revoked API token tf2-x34 (apitok_95d2eddb1ce2c0ab9035fbbea47b020b).
## D: versions, restore from the other OS
L versions: VERSION  KIND      SAVED             AUTHOR                             CHANGES  MESSAGE / 22       saved     2026-11-13 01:00  user:<owner>  +0 -0    Tags of lanex2-w changed / 21       saved     2026-11-13 01:00  user:<owner>
W policy restore 17 rc 0 Saved policy version 23 (sha256:4cd372b363e5493a179e2d0b20deb5c6510ac9c7e74c646acc493271fbec1d83).
after restore from W, L sees digest equal to baseline: True version 23
W versions: VERSION  KIND      SAVED             AUTHOR                             CHANGES  MESSAGE / 23       restored  2026-11-13 01:01  user:<owner>  +0 -0    Restored version 17 / 22       saved     2026-11-13 01:00  user:<owner>
## Result
Passed on L and W: UTF-16 BOM CRLF file accepted by test, set --dry-run and set --yes from W (saved, comments kept, same digest seen by L); the same file accepted from L; identical explain text for 7 action/target pairs and for who-can; identical refusal text for an invalid policy and no half-save (version unchanged); `pb machine tag` run on W applies to the L machine and L's `machine tag` / `machine list --tag` agree (and L tagging the W machine, seen from W); versions agree; restore run on W is seen by L with the baseline digest; a scoped API token's decisions (`outside_token_scope` denial) are identical from L and W.
Gaps (so X-34 stays open): the deciding rule behind a real deny for a non-owner (owner access always answers `owner`); the refusal text of a live exec/ssh/files attempt by a non-owner on each OS; private_access ports and ENV rules (team only); an API token signed in on each OS (needs throwaway machines of each OS); the Mac pairs.
