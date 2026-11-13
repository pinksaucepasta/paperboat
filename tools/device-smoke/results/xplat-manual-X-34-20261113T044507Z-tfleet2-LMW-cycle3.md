# T-FLEET2: X-34 L, M and W as clients and targets (personal workspace of the fleet account) on dev-051c1f7f2, 2026-11-13 10:11-10:14 IST

lanex2-l (Linux), lanex2-m (macOS, pbmac VM) and lanex2-w (Windows 11, the real Victus PC): `pb version` dev-051c1f7f2 on all three (the first line below). The owner's address is shown as <owner>.
Fixture: the personal policy had version 105 (digest sha256:4cd372b3...); restored from the Mac at the end (digest equal on all three). The Windows file is written by PowerShell `Out-File -Encoding Unicode` (UTF-16 LE BOM, CRLF); the Linux and macOS files are UTF-8. Tags and group in the file name all three fleet machines; one rule allows terminal, exec, managed_ssh, files, preview_manage, tunnel_manage on the tag; one in-file test.
Not covered here (personal workspace cannot show them): a rule-decided deny for a non-owner (the owner always answers `owner`), private_access ports and ENV rules (team only), a live refusal text of a non-owner. A Codex sub-agent is repairing the team member fixture for those (an earlier run found the member's team membership inactive).

## versions {'L': 'dev-051c1f7f2', 'M': 'dev-051c1f7f2', 'W': 'dev-051c1f7f2'}
baseline policy version 105 sha256:4cd372b363e5493a179e2d0b20deb5c6510ac9c7e74c646acc493271fbec1d83
W file (UTF-16 BOM CRLF): size 1204 first bytes 255,254
## A: files, each client
L policy test rc 0 Tests: 1 passed, 0 failed / No access changes. | set --dry-run rc 0
M policy test rc 0 Tests: 1 passed, 0 failed / No access changes. | set --dry-run rc 0
W policy test rc 0 Tests: 1 passed, 0 failed / No access changes. | set --dry-run rc 0
M policy set --yes rc 0 No access changes. / Saved policy version 106 (sha256:745ff716a06242b5653c06c34336978a3b0a345890e1efd0479264289165b07c).
after M save: versions {'L': 106, 'M': 106, 'W': 106} digests equal True comment kept True
W policy set --yes (UTF-16) rc 0  Saved policy version 107 (sha256:8fc892cf01e7d6de48f650a4cedd7d6ae190826a6dad0651cb11c116396c15bf).
after W save: versions {'L': 107, 'M': 107, 'W': 107} digests equal True
## B: explain/test text, three clients
explain exec machine:lanex2-m | 0 ALLOWED: owner | identical across L,M,W: True
explain files machine:lanex2-m | 0 ALLOWED: owner | identical across L,M,W: True
explain terminal machine:lanex2-l | 0 ALLOWED: owner | identical across L,M,W: True
explain managed_ssh machine:lanex2-w | 0 ALLOWED: owner | identical across L,M,W: True
explain preview_manage machine:lanex2-m | 0 ALLOWED: owner | identical across L,M,W: True
explain tunnel_manage machine:lanex2-w | 0 ALLOWED: owner | identical across L,M,W: True
explain exec machine:lanex2-l | 0 ALLOWED: owner | identical across L,M,W: True
policy test (saved): (0, 'Tests: 1 passed, 0 failed\nNo access changes.') identical True
who can exec lanex2-m identical: True
invalid file refusal: rc {'L': 2, 'M': 2, 'W': 2} same text: True | /rules/0/dst/0: unknown_machine: machine "nope" is not in this workspace / Tests: 0 passed, 0 failed / N
version unchanged after refused saves: {'L': 107, 'M': 107, 'W': 107}
## C: tags from the other OS
M tag lanex2-l add tfa rc 0 Tags of lanex2-l: tag:tf2, tag:tfa (policy version 108).
  machine list --tag tfa seen from the others: {'L': ['lanex2-l'], 'W': ['lanex2-l']}
  removed rc 0
L tag lanex2-m add tfb rc 0 Tags of lanex2-m: tag:tf2, tag:tfb (policy version 110).
  machine list --tag tfb seen from the others: {'M': ['lanex2-m'], 'W': ['lanex2-m']}
  removed rc 0
M tag lanex2-w add tfc rc 0 Tags of lanex2-w: tag:tf2, tag:tfc (policy version 112).
  machine list --tag tfc seen from the others: {'L': ['lanex2-w'], 'W': ['lanex2-w']}
  removed rc 0
W tag lanex2-m add tfd rc 0 Tags of lanex2-m: tag:tf2, tag:tfd (policy version 114).
  machine list --tag tfd seen from the others: {'L': ['lanex2-m'], 'M': ['lanex2-m']}
  removed rc 0
## C2: scoped API token explain, three clients
create scoped token rc 0
token explain exec machine:lanex2-m | 0 ALLOWED: owner | identical: True
token explain files machine:lanex2-m | 0 DENIED: outside_token_scope | identical: True
token explain exec machine:lanex2-w | 0 ALLOWED: owner | identical: True
revoke tf2-x34m rc 0 Revoked API token tf2-x34m (apitok_f356a1ddd7b2aa1193c10f9b673c387e).
## D: restore from the Mac, seen elsewhere
M policy restore 105 rc 0 Saved policy version 116 (sha256:4cd372b363e5493a179e2d0b20deb5c6510ac9c7e74c646acc493271fbec1d83).
after restore from M, digest equals baseline on L,M,W: True {'L': 116, 'M': 116, 'W': 116}
versions (number, kind) seen from L, M and W identical: True
## Result
Passed on L, M and W: policy file test and set --dry-run on each OS; a save from the Mac and a UTF-16 BOM CRLF save from Windows are seen with the same digest on all three, comments kept; identical explain text on all three clients for 7 action/target pairs and for who-can; identical refusal text and no half-save for an invalid file (version unchanged); `pb machine tag` run on one OS applies to a machine of another OS and `pb machine list --tag` from the other two agrees (M>L, L>M, M>W, W>M); a scoped API token's decision (`outside_token_scope`) is identical from all three; restore run on the Mac is seen on all three with the baseline digest; the version list (number, kind) is identical on all three.
