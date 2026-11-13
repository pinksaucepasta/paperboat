# X-08 L<->W by hand on own machines, 2026-11-13 IST 13:50 (results/xplat-manual-X-08-LW-20261113T0820Z.md)

Machines (own, nothing shared): `rx08-l` (Linux container on hp, machine_cda8ae7b) and `rx08-w3` (hosted Windows runner winpool-4, machine_173d3956), one account, one scratch repository `xplat-config-fixture-rx08` (HTTP repository on hp, both machines `bidirectional`, automatic updates on).
`pb version`: rx08-l dev-918afefe9, rx08-w3 dev-918afefe9 (installed from /home/ubuntu/pb-builds/dev-918afefe9 on 11-13 12:20 IST, read again by the harness every run).

Commands per flow (every pb command run on the named device through the harness): the same line of a synced file is written on both devices (winner first, loser `delay` seconds later; the device that really holds the conflict is the loser), then
`pb config conflict list --json` on both devices, `pb config conflict show <loser> <path>` on both, `pb config conflict compare <loser> <path> --json` on both (the winner compares over the encrypted peer path), then `pb config conflict resolve <loser> <path> --keep repository|machine` or `pb config force pull|push <loser> [<path>]` (confirmation preview + `--confirm`) run on the OTHER device; success = bytes equal on both devices and in the repository, conflict list empty.

## Conflict flows (the device that really lost the race is named in each line; loser W: u1 u2 u5 u6 v1 v3 v4; loser L: v2 and the w/x flows below)

- [12:44:08] u1: winner L, loser W (rx08-w3). `conflict list` from L/W = {'L': [('rx08-w3', 'concurrent_update', 'e34fd72306f0')], 'W': [('rx08-w3', 'concurrent_update', 'e34fd72306f0')]}; `conflict show` revision from L/W = {'L': 'e34fd72306f0', 'W': 'e34fd72306f0'}. Listed once, same revision on both: True
- [12:44:58] u1: `conflict compare rx08-w3` run on rx08-l: ok on try 4, machine version b'one\nsame line EDIT-W u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u1\nthree\n' == repository head True
- [12:45:39] u1: `conflict compare rx08-w3` run on rx08-w3: ok on try 2, machine version b'one\nsame line EDIT-W u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u1\nthree\n' == repository head True
- [12:46:34] u1: `conflict resolve rx08-w3 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-L u1\nthree\n'; conflict list empty on both: True
- [12:46:34] u1 RESULT PASS
- [12:49:55] u2: winner L, loser W (rx08-w3). `conflict list` from L/W = {'L': [('rx08-w3', 'merge_conflict', 'e099ec1299ba')], 'W': [('rx08-w3', 'merge_conflict', 'e099ec1299ba')]}; `conflict show` revision from L/W = {'L': 'e099ec1299ba', 'W': 'e099ec1299ba'}. Listed once, same revision on both: True
- [12:50:21] u2: `conflict compare rx08-w3` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-W u2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u2\nthree\n' == repository head True
- [12:50:38] u2: `conflict compare rx08-w3` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-W u2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u2\nthree\n' == repository head True
- [12:52:31] u2: `conflict resolve rx08-w3 --keep machine` run on rx08-l -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-W u2\nthree\n'; conflict list empty on both: True
- [12:52:31] u2 RESULT PASS
- [12:56:26] v1: winner L loser W: `config force pull rx08-w3 cfg/app/x08-v1.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L v1\nthree\n'; conflict list empty: True
- [12:56:26] v1 RESULT PASS
- [13:01:24] v2: winner W loser L: `config force push rx08-l cfg/app/x08-v2.txt` run on rx08-w3 -> queued; bytes on both and repository b'one\nsame line EDIT-L v2\nthree\n'; conflict list empty: True
- [13:01:24] v2 RESULT PASS
- [13:04:47] u5: winner L, loser W (rx08-w3). `conflict list` from L/W = {'L': [('rx08-w3', 'concurrent_update', '59883b93a235')], 'W': [('rx08-w3', 'concurrent_update', '59883b93a235')]}; `conflict show` revision from L/W = {'L': '59883b93a235', 'W': '59883b93a235'}. Listed once, same revision on both: True
- [13:05:21] u5: `conflict compare rx08-w3` run on rx08-l: ok on try 3, machine version b'one\nsame line EDIT-W u5\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u5\nthree\n' == repository head True
- [13:05:34] u5: `conflict compare rx08-w3` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-W u5\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u5\nthree\n' == repository head True
- [13:06:26] u5: `conflict resolve rx08-w3 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-L u5\nthree\n'; conflict list empty on both: True
- [13:06:26] u5 RESULT PASS
- [13:10:01] u6: winner L, loser W (rx08-w3). `conflict list` from L/W = {'L': [('rx08-w3', 'concurrent_update', '2e74174524b3')], 'W': [('rx08-w3', 'concurrent_update', '2e74174524b3')]}; `conflict show` revision from L/W = {'L': '2e74174524b3', 'W': '2e74174524b3'}. Listed once, same revision on both: True
- [13:10:23] u6: `conflict compare rx08-w3` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-W u6\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u6\nthree\n' == repository head True
- [13:10:36] u6: `conflict compare rx08-w3` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-W u6\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u6\nthree\n' == repository head True
- [13:12:32] u6: `conflict resolve rx08-w3 --keep machine` run on rx08-l -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-W u6\nthree\n'; conflict list empty on both: True
- [13:12:32] u6 RESULT PASS
- [13:17:27] v3: winner L loser W: `config force push rx08-w3` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-W v3\nthree\n'; conflict list empty: True
- [13:17:27] v3 RESULT PASS
- [13:21:32] v4: winner L loser W: `config force pull rx08-w3` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L v4\nthree\n'; conflict list empty: True
- [13:21:32] v4 RESULT PASS

### Loser L, Windows edits first (w1 w3 resolve --keep repository, w2 w4 resolve --keep machine, x1 x2 force pull of the path)

- [13:54:26] w1: winner W, loser L (rx08-l). `conflict list` from L/W = {'L': [('rx08-l', 'merge_conflict', 'fd30c9f797ea')], 'W': [('rx08-l', 'merge_conflict', 'fd30c9f797ea')]}; `conflict show` revision from L/W = {'L': 'fd30c9f797ea', 'W': 'fd30c9f797ea'}. Listed once, same revision on both: True
- [13:54:41] w1: `conflict compare rx08-l` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-L w1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w1\nthree\n' == repository head True
- [13:54:48] w1: `conflict compare rx08-l` run on rx08-l: ok on try 1, machine version b'one\nsame line EDIT-L w1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w1\nthree\n' == repository head True
- [13:55:42] w1: `conflict resolve rx08-l --keep repository` run on rx08-w3 -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-W w1\nthree\n'; conflict list empty on both: True
- [13:55:42] w1 RESULT PASS
- [13:59:55] w2: winner W, loser L (rx08-l). `conflict list` from L/W = {'L': [('rx08-l', 'merge_conflict', '1725f85008ee')], 'W': [('rx08-l', 'merge_conflict', '1725f85008ee')]}; `conflict show` revision from L/W = {'L': '1725f85008ee', 'W': '1725f85008ee'}. Listed once, same revision on both: True
- [14:00:07] w2: `conflict compare rx08-l` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-L w2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w2\nthree\n' == repository head True
- [14:00:12] w2: `conflict compare rx08-l` run on rx08-l: ok on try 1, machine version b'one\nsame line EDIT-L w2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w2\nthree\n' == repository head True
- [14:01:40] w2: `conflict resolve rx08-l --keep machine` run on rx08-w3 -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-L w2\nthree\n'; conflict list empty on both: True
- [14:01:40] w2 RESULT PASS
- [14:05:57] w3: winner W, loser L (rx08-l). `conflict list` from L/W = {'L': [('rx08-l', 'merge_conflict', '7a32c9338ed9')], 'W': [('rx08-l', 'merge_conflict', '7a32c9338ed9')]}; `conflict show` revision from L/W = {'L': '7a32c9338ed9', 'W': '7a32c9338ed9'}. Listed once, same revision on both: True
- [14:06:10] w3: `conflict compare rx08-l` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-L w3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w3\nthree\n' == repository head True
- [14:06:27] w3: `conflict compare rx08-l` run on rx08-l: ok on try 3, machine version b'one\nsame line EDIT-L w3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w3\nthree\n' == repository head True
- [14:07:37] w3: `conflict resolve rx08-l --keep repository` run on rx08-w3 -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-W w3\nthree\n'; conflict list empty on both: True
- [14:07:37] w3 RESULT PASS
- [14:11:11] w4: winner W, loser L (rx08-l). `conflict list` from L/W = {'L': [('rx08-l', 'merge_conflict', '28633f5a8fb9')], 'W': [('rx08-l', 'merge_conflict', '28633f5a8fb9')]}; `conflict show` revision from L/W = {'L': '28633f5a8fb9', 'W': '28633f5a8fb9'}. Listed once, same revision on both: True
- [14:11:24] w4: `conflict compare rx08-l` run on rx08-w3: ok on try 1, machine version b'one\nsame line EDIT-L w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w4\nthree\n' == repository head True
- [14:11:29] w4: `conflict compare rx08-l` run on rx08-l: ok on try 1, machine version b'one\nsame line EDIT-L w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W w4\nthree\n' == repository head True
- [14:13:39] w4: `conflict resolve rx08-l --keep machine` run on rx08-w3 -> queued; bytes on rx08-l/rx08-w3 and repository all b'one\nsame line EDIT-L w4\nthree\n'; conflict list empty on both: True
- [14:13:39] w4 RESULT PASS
- [14:18:39] x1: winner W loser L: `config force pull rx08-l cfg/app/x08-x1.txt` run on rx08-w3 -> queued; bytes on both and repository b'one\nsame line EDIT-W x1\nthree\n'; conflict list empty: True
- [14:18:39] x1 RESULT PASS
- [14:23:46] x2: winner W loser L: `config force pull rx08-l cfg/app/x08-x2.txt` run on rx08-w3 -> queued; bytes on both and repository b'one\nsame line EDIT-W x2\nthree\n'; conflict list empty: True
- [14:23:46] x2 RESULT PASS

## `pb config approve` (automatic updates off on the target, the other device edits, approve run on the other device right after the review is for the exact repository head)

- [13:44:07] a1: automatic updates off on rx08-w3; edit on rx08-l -> rx08-w3 state pending error_code review_required review ['', '', '', '', '', '', '', '', '', '', '', '', '']; rx08-w3 file unchanged before approval: True
- [13:44:09] a1: status before approve ('pending', 'review_required', 'ccfc065cd6', '', 18); approve output {"assignment": {"configuration_revision": "f5a4902a3a469d212b78e285c68fd53937c32fc097abfe82ee3860d119524025", "path_rules": [{"source": "os", "id": "os:cfg/app"
- [13:44:30] a1: status after approve ('syncing', None, '', '', 19)
- [13:44:51] a1: status after approve ('healthy', None, 'ccfc065cd6', 'ccfc065cd6', 19)
- [13:45:12] a1: status after approve ('healthy', None, 'ccfc065cd6', 'ccfc065cd6', 19)
- [13:46:03] a1: `config approve rx08-w3` run on rx08-l -> {...} head True)
- [13:46:04] a1 RESULT PASS
- [13:47:48] a2: automatic updates off on rx08-l; edit on rx08-w3 -> rx08-l state pending error_code review_required review ['', '', '', '', '', '', '', '', '', '', '', '', '']; rx08-l file unchanged before approval: True
- [13:47:57] a2: status before approve ('pending', 'review_required', '6a558f3f72', '', 35); approve output {"assignment": {"configuration_revision": "c21102260aca58c88186fe265ef242075e9706bbdecca5c5cbde230205f78885", "path_rules": [{"source": "os", "id": "os:cfg/app"
- [13:48:22] a2: status after approve ('healthy', None, '6a558f3f72', '6a558f3f72', 36)
- [13:48:46] a2: status after approve ('healthy', None, '6a558f3f72', '6a558f3f72', 36)
- [13:49:10] a2: status after approve ('healthy', None, '6a558f3f72', '6a558f3f72', 36)
- [13:49:17] a2: `config approve rx08-l` run on rx08-w3 -> {...} head True)
- [13:49:18] a2 RESULT PASS

## Notes
- Earlier attempts of the approve part on dev-e4d2cc0f8 and on a first run here failed in the harness: it approved the review that was already pending from an older commit (state review_required came 1 s after the edit with 13 review entries), so the approved head was older than the edit; `pb config approve` then applied exactly that head (status healthy, file unchanged) and the edit arrived as a new review_required ~50 s later (repro: `r9` run, review_required for revision da43425b73, new review_required 38ea65535e at +50 s). That is the exact-head behaviour the row asks for, not a product bug. The harness now waits for review_required with `remote_revision` equal to the repository head that contains the edit.
- Conflict reasons seen: merge_conflict (first listing) and concurrent_update; both were listed once per device with the same revision from either CLI.
- Env-wide `force pull` (v4 with loser L) timed out on dev-e4d2cc0f8 (failures.md) and passed on dev-051c1f7f2 and on dev-918afefe9 (this file).
