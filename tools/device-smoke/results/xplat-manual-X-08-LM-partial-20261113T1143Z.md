# X-08 L<->M by hand on own machines, 2026-11-13 IST 14:25-16:05 (PARTIAL, not ticked)

Machines: `rx08-l` (Linux container on hp) and `rx08-m2` (hosted macOS runner macpool-5), one account, one scratch repository, both `bidirectional`; `pb version` dev-918afefe9 on both (installed from /home/ubuntu/pb-builds/dev-918afefe9 at 14:00 IST). Same harness and commands as the L<->W file (xplat-manual-X-08-LW-*.md).

## What passed (loser = the machine that really held the conflict)

- Loser M (Mac) with Linux driving: `conflict list` once with the same revision from both CLIs, `show`, `compare` byte-exact from both, `resolve --keep repository` (u1 u5 w1 w3) and `--keep machine` (u6 w4), `force pull` path (v1 x1) and env scope (v4), `force push` path (v2) and env scope (v3): all PASS.
- Loser L (Linux): only u2 (`resolve --keep machine`, M edited first) PASS. The Mac wins the publish race only rarely, so `resolve --keep repository`, `force pull` and `force push` with L as loser were NOT seen on this build (y1-y6 never produced a Linux loser; w2 was a harness JSON error and passed on rerun w4).
- Not run on this build: `config approve` L<->M (the runner macpool-5 was cancelled by another lane at ~15:55 IST; the next macOS runner request waited more than 15 minutes in the hosted queue), and all of M<->W.

## Transcript lines

- [14:28:11] u1: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', '2c9a53b1120d')], 'M': [('rx08-m2', 'merge_conflict', '2c9a53b1120d')]}; `conflict show` revision from L/M = {'L': '2c9a53b1120d', 'M': '2c9a53b1120d'}. Listed once, same revision on both: True
- [14:28:36] u1: `conflict compare rx08-m2` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-M u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u1\nthree\n' == repository head True
- [14:29:36] u1: `conflict compare rx08-m2` run on rx08-m2: ok on try 4, machine version b'one\nsame line EDIT-M u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u1\nthree\n' == repository head True
- [14:30:38] u1: `conflict resolve rx08-m2 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-L u1\nthree\n'; conflict list empty on both: True
- [14:30:38] u1 RESULT PASS
- [14:32:56] u2: winner M, loser L (rx08-l). `conflict list` from L/M = {'L': [('rx08-l', 'merge_conflict', 'ffa51c499a30')], 'M': [('rx08-l', 'merge_conflict', 'ffa51c499a30')]}; `conflict show` revision from L/M = {'L': 'ffa51c499a30', 'M': 'ffa51c499a30'}. Listed once, same revision on both: True
- [14:33:44] u2: `conflict compare rx08-l` run on rx08-m2: ok on try 2, machine version b'one\nsame line EDIT-L u2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u2\nthree\n' == repository head True
- [14:33:57] u2: `conflict compare rx08-l` run on rx08-l: ok on try 1, machine version b'one\nsame line EDIT-L u2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u2\nthree\n' == repository head True
- [14:35:44] u2: `conflict resolve rx08-l --keep machine` run on rx08-m2 -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-L u2\nthree\n'; conflict list empty on both: True
- [14:35:44] u2 RESULT PASS
- [14:39:36] v1: winner L loser M: `config force pull rx08-m2 cfg/app/x08-v1.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L v1\nthree\n'; conflict list empty: True
- [14:39:36] v1 RESULT PASS
- [14:44:36] v2: winner L loser M: `config force push rx08-m2 cfg/app/x08-v2.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-M v2\nthree\n'; conflict list empty: True
- [14:44:36] v2 RESULT PASS
- [14:48:05] u5: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', 'd0cd1a2abf43')], 'M': [('rx08-m2', 'merge_conflict', 'd0cd1a2abf43')]}; `conflict show` revision from L/M = {'L': 'd0cd1a2abf43', 'M': 'd0cd1a2abf43'}. Listed once, same revision on both: True
- [14:48:45] u5: `conflict compare rx08-m2` run on rx08-l: ok on try 3, machine version b'one\nsame line EDIT-M u5\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u5\nthree\n' == repository head True
- [14:48:59] u5: `conflict compare rx08-m2` run on rx08-m2: ok on try 1, machine version b'one\nsame line EDIT-M u5\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u5\nthree\n' == repository head True
- [14:49:43] u5: `conflict resolve rx08-m2 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-L u5\nthree\n'; conflict list empty on both: True
- [14:49:43] u5 RESULT PASS
- [14:53:10] u6: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', '74c363dfd840')], 'M': [('rx08-m2', 'merge_conflict', '74c363dfd840')]}; `conflict show` revision from L/M = {'L': '74c363dfd840', 'M': '74c363dfd840'}. Listed once, same revision on both: True
- [14:53:31] u6: `conflict compare rx08-m2` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-M u6\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u6\nthree\n' == repository head True
- [14:54:12] u6: `conflict compare rx08-m2` run on rx08-m2: ok on try 3, machine version b'one\nsame line EDIT-M u6\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u6\nthree\n' == repository head True
- [14:55:36] u6: `conflict resolve rx08-m2 --keep machine` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-M u6\nthree\n'; conflict list empty on both: True
- [14:55:36] u6 RESULT PASS
- [14:59:06] w1: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', 'c1822161c50d')], 'M': [('rx08-m2', 'merge_conflict', 'c1822161c50d')]}; `conflict show` revision from L/M = {'L': 'c1822161c50d', 'M': 'c1822161c50d'}. Listed once, same revision on both: True
- [14:59:55] w1: `conflict compare rx08-m2` run on rx08-l: ok on try 4, machine version b'one\nsame line EDIT-M w1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w1\nthree\n' == repository head True
- [15:00:55] w1: `conflict compare rx08-m2` run on rx08-m2: ok on try 4, machine version b'one\nsame line EDIT-M w1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w1\nthree\n' == repository head True
- [15:01:39] w1: `conflict resolve rx08-m2 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-L w1\nthree\n'; conflict list empty on both: True
- [15:01:39] w1 RESULT PASS
- [15:05:08] w2: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', '7a96dc9c4002')], 'M': [('rx08-m2', 'merge_conflict', '7a96dc9c4002')]}; `conflict show` revision from L/M = {'L': '7a96dc9c4002', 'M': '7a96dc9c4002'}. Listed once, same revision on both: True
- [15:05:38] w2: `conflict compare rx08-m2` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-M w2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w2\nthree\n' == repository head True
- [15:06:45] w2: `conflict compare rx08-m2` run on rx08-m2: ok on try 4, machine version b'one\nsame line EDIT-M w2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w2\nthree\n' == repository head True
- [15:08:06] w2 RESULT FAIL KeyError('conflicts')
- [15:13:13] w3: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'concurrent_update', 'be62c6cf7be3')], 'M': [('rx08-m2', 'concurrent_update', 'be62c6cf7be3')]}; `conflict show` revision from L/M = {'L': 'be62c6cf7be3', 'M': 'be62c6cf7be3'}. Listed once, same revision on both: True
- [15:13:38] w3: `conflict compare rx08-m2` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-M w3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w3\nthree\n' == repository head True
- [15:14:05] w3: `conflict compare rx08-m2` run on rx08-m2: ok on try 2, machine version b'one\nsame line EDIT-M w3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w3\nthree\n' == repository head True
- [15:14:37] w3: `conflict resolve rx08-m2 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-L w3\nthree\n'; conflict list empty on both: True
- [15:14:37] w3 RESULT PASS
- [15:18:11] w4: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', '5a0c232bc90e')], 'M': [('rx08-m2', 'merge_conflict', '5a0c232bc90e')]}; `conflict show` revision from L/M = {'L': '5a0c232bc90e', 'M': '5a0c232bc90e'}. Listed once, same revision on both: True
- [15:18:33] w4: `conflict compare rx08-m2` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-M w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w4\nthree\n' == repository head True
- [15:18:59] w4: `conflict compare rx08-m2` run on rx08-m2: ok on try 2, machine version b'one\nsame line EDIT-M w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L w4\nthree\n' == repository head True
- [15:20:34] w4: `conflict resolve rx08-m2 --keep machine` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-M w4\nthree\n'; conflict list empty on both: True
- [15:20:34] w4 RESULT PASS
- [15:24:33] x1: winner L loser M: `config force pull rx08-m2 cfg/app/x08-x1.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L x1\nthree\n'; conflict list empty: True
- [15:24:33] x1 RESULT PASS
- [15:28:37] x2: winner L loser M: `config force pull rx08-m2 cfg/app/x08-x2.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L x2\nthree\n'; conflict list empty: True
- [15:28:37] x2 RESULT PASS
- [15:33:39] v3: winner L loser M: `config force push rx08-m2` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-M v3\nthree\n'; conflict list empty: True
- [15:33:39] v3 RESULT PASS
- [15:37:36] v4: winner L loser M: `config force pull rx08-m2` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L v4\nthree\n'; conflict list empty: True
- [15:37:36] v4 RESULT PASS
- [15:41:10] y1: winner L, loser M (rx08-m2). `conflict list` from L/M = {'L': [('rx08-m2', 'merge_conflict', 'be035f985148')], 'M': [('rx08-m2', 'merge_conflict', 'be035f985148')]}; `conflict show` revision from L/M = {'L': 'be035f985148', 'M': 'be035f985148'}. Listed once, same revision on both: True
- [15:41:33] y1: `conflict compare rx08-m2` run on rx08-l: ok on try 2, machine version b'one\nsame line EDIT-M y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L y1\nthree\n' == repository head True
- [15:42:31] y1: `conflict compare rx08-m2` run on rx08-m2: ok on try 4, machine version b'one\nsame line EDIT-M y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L y1\nthree\n' == repository head True
- [15:43:40] y1: `conflict resolve rx08-m2 --keep repository` run on rx08-l -> queued; bytes on rx08-l/rx08-m2 and repository all b'one\nsame line EDIT-L y1\nthree\n'; conflict list empty on both: True
- [15:43:40] y1 RESULT PASS
- [15:47:35] y2: winner L loser M: `config force pull rx08-m2 cfg/app/x08-y2.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-L y2\nthree\n'; conflict list empty: True
- [15:47:35] y2 RESULT PASS
- [15:52:37] y3: winner L loser M: `config force push rx08-m2 cfg/app/x08-y3.txt` run on rx08-l -> queued; bytes on both and repository b'one\nsame line EDIT-M y3\nthree\n'; conflict list empty: True
- [15:52:37] y3 RESULT PASS
- [15:57:44] y4 RESULT FAIL AssertionError('timed out waiting for base y4 on both')
- [15:57:44] y5 RESULT FAIL AssertionError('channel 0: open failed: connect failed: ssh-monitor: dial tcp 127.0.0.1:22305: connect: connection refused\r\nstdio forwarding failed\r\nConnection closed by UNKNOWN port 65535\r\n')
- [15:57:44] y6 RESULT FAIL AssertionError('channel 0: open failed: connect failed: ssh-monitor: dial tcp 127.0.0.1:22305: connect: connection refused\r\nstdio forwarding failed\r\nConnection closed by UNKNOWN port 65535\r\n')
