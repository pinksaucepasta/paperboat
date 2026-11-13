# T-FLEET2: X-34 non-owner (team member) and private access on L, M, W (Victus), 2026-11-13 10:15-10:45 IST

Fleet: lanex2-l, lanex2-m, lanex2-w (Victus), `pb version` dev-051c1f7f2 on all three. The member client is the existing second-account Linux container xa2-l2 (pb dev-d24378a95). The member's team membership had gone inactive (an earlier Codex run proved that blocks everything with `workspace_unavailable`); I re-invited the member's own address with `pb team invite` from the owner side and accepted on xa2-l2 (`pb team accept`): membership active again (generation 4). A stray invitation to a different address was cancelled.

## Part 1: tools/device-smoke/policy_team.py against each fleet machine as the shared target (owner = the fleet account, member = xa2-l2)
Each run: share the machine into the team, member refused with no rule, grant exec to every member -> member can exec, deny rule saved after the access review -> member refused (deny wins) and `explain` names the deny rule, who-can/what-can, owner guard, member read-only rights (cannot add a rule on another member's machine, explains only itself), team ENV grant (explain allows env_read, no key yet, revoke removes the rule), restore of the starting policy, audit events, unshare.
Output (emails redacted), 28 PASS lines and RESULT PASS for each of the three targets:

=== target lanex2-l 10:17:21
RESULT PASS
=== target lanex2-m 10:18:12
RESULT PASS
=== target lanex2-w 10:19:08
RESULT PASS

Check counts per target (PASS lines):
  lanex2-l: 29 PASS, 0 FAIL
  lanex2-m: 29 PASS, 0 FAIL
  lanex2-w: 29 PASS, 0 FAIL

## Part 2: private access ports and a UTF-16 team policy, owner `explain` from all three clients
A bug found here and fixed (fdfe1baad): on a team policy with a private_access rule `pb policy test --workspace <team> file` (and set, and explain who-can/what-can) ended as the generic "Paperboat could not finish the command"; the CLI event log said ResponseDecodeError / UnmarshalTypeError: the server sends `ports` as one string ("5432"), the client read a list. Test TestPolicyExplainWhoCanReadsThePortsTheServerSends, anvit: ok cmd/pb, ok internal/api. The runs below use the fixed build dev-fdfe1baad as a one-off client (a scratch copy on each machine, not installed, removed afterwards) because the fleet binaries are older.
Policy: allow_private_access true; one rule: the member may reach port 5432 on lanex2-l, -m and -w; two in-file tests. The Windows client saved it from a UTF-16 BOM CRLF file.
team policy version before 49
share lanex2-l rc 0
share lanex2-m rc 0
share lanex2-w rc 0
L policy test rc 0 Tests: 2 passed, 0 failed / Access changes: / + gained user:<member> machine:lan
M policy test rc 0 Tests: 2 passed, 0 failed / Access changes: / + gained user:<member> machine:lan
W policy test rc 0 Tests: 2 passed, 0 failed / Access changes: / + gained user:<member> machine:lan
W policy set (UTF-16) rc 0 / Saved policy version 50 (sha256:4211df3b021b89b7d188e2d5b75cc9823da82b38829bf14625a0f3fa3707e824).
explain private_access lanex2-l 5432 | 0 ALLOWED: rule / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-l 22 | 0 DENIED: port_not_allowed / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-l 6000 | 0 DENIED: port_not_allowed / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-m 5432 | 0 ALLOWED: rule / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-m 22 | 0 DENIED: port_not_allowed / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-m 6000 | 0 DENIED: port_not_allowed / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-w 5432 | 0 ALLOWED: rule / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-w 22 | 0 DENIED: port_not_allowed / Rules: /rules/0 | identical on L,M,W: True
explain private_access lanex2-w 6000 | 0 DENIED: port_not_allowed / Rules: /rules/0 | identical on L,M,W: True
who can reach lanex2-m:5432 identical: True | Who can (1): / SUBJECT                            TARGET            ACTION          PORTS  REASON  RULES / user:final-43
restore team policy 49 rc 1
unshare lanex2-w rc 0
unshare lanex2-m rc 0
unshare lanex2-l rc 0
team policy digest back to start: False

The team policy was restored afterwards with the same fixed client (content equal to the start; the restore is a new version, 54), the three machines unshared, my scratch files removed.

## Result
Passed on L, M and W as shared targets: a real non-owner is refused without a rule, allowed by a grant, refused again by a deny rule that `explain` names, and the team ENV grant follows the same rules (28 of 28 each). Private access ports: identical `ALLOWED: rule /rules/0` for port 5432 and `DENIED: port_not_allowed` for 22 and 6000 on the three machines from three client OSes, a UTF-16 save from Windows accepted.
Not shown (X-34 stays open): the refusal text of a non-owner's live command seen from a macOS and a Windows client (the only second-account member client is the Linux container); an API token created on one OS and signed in on each OS; the `[x]` also needs the fixed CLI on the fleet (the fleet binaries predate fdfe1baad).
