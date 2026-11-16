# X-08 M<->W: conflicts across macOS and Windows, by hand on own macpool and winpool runners, 2026-11-16 IST (R-X-08C)

Machines: an own macOS runner rx08c-m (macpool-4, Apple silicon, user runner) and own Windows runners rx08c-w (winpool-4) / rx08c-w2 (winpool-1; the first runner expired after its 4 h slot), both paired into the shared test account, because the fleet Mac and victus were unreachable on the saturated phone hotspot. Same method as results/xplat-manual-X-08-LM-20261116.md: a scratch repository on anvit, one config-sync assignment per device (cfg/app/*.txt, %APPDATA%/xplat/app on Windows, ~/Library/Application Support/xplat/app on macOS), the same line edited on both, conflict list from both devices (once, same revision), show, compare byte-exact from both, then the resolution or force command from the other device, equal bytes on both and the repository, conflict list empty. M is the winner (W loses) in u1 u3 v1 p1 p2 v3 a1, W is the winner (M loses) in u2 u4 v2 v5 v4 y6 a2. The Windows runner is non-elevated ssh; files written over ssh. A flow that failed on a runner problem (expired runner, ssh reset, first-sync race noted in failures.md) was rerun, only passes are listed. Per the 2026-11-16 speed rules parts passed on different builds; config sync code did not change between them.

## u1: W loser: resolve --keep repository (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 16:46:33-17:05:17 IST)

- [16:52:46] u1: winner rx08c-m reached the repository 29 s after its edit
- [16:54:20] u1: winner M, loser W (rx08c-w). `conflict list` from M/W = {'M': [('rx08c-w', 'merge_conflict', 'd222c552a446')], 'W': [('rx08c-w', 'merge_conflict', 'd222c552a446')]}; `conflict show` revision from M/W = {'M': 'd222c552a446', 'W': 'd222c552a446'}. Listed once, same revision on both: True
- [16:54:34] u1: `conflict compare rx08c-w` run on rx08c-m: ok on try 1, machine version b'one\nsame line EDIT-W u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u1\nthree\n' == repository head True
- [16:54:48] u1: `conflict compare rx08c-w` run on rx08c-w: ok on try 1, machine version b'one\nsame line EDIT-W u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u1\nthree\n' == repository head True
- [16:55:32] u1: `conflict resolve rx08c-w --keep repository` run on rx08c-m -> queued; bytes on rx08c-m/rx08c-w and repository all b'one\nsame line EDIT-M u1\nthree\n'; conflict list empty on both: True
- [16:55:32] u1 RESULT PASS

## u3: W loser: resolve --keep machine (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 17:05:17-17:22:35 IST)

- [17:11:57] u3: winner rx08c-m reached the repository 34 s after its edit
- [17:13:28] u3: winner M, loser W (rx08c-w). `conflict list` from M/W = {'M': [('rx08c-w', 'merge_conflict', '6a40202ba125')], 'W': [('rx08c-w', 'merge_conflict', '6a40202ba125')]}; `conflict show` revision from M/W = {'M': '6a40202ba125', 'W': '6a40202ba125'}. Listed once, same revision on both: True
- [17:13:52] u3: `conflict compare rx08c-w` run on rx08c-m: ok on try 2, machine version b'one\nsame line EDIT-W u3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u3\nthree\n' == repository head True
- [17:14:03] u3: `conflict compare rx08c-w` run on rx08c-w: ok on try 1, machine version b'one\nsame line EDIT-W u3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u3\nthree\n' == repository head True
- [17:16:11] u3: `conflict resolve rx08c-w --keep machine` run on rx08c-m -> queued; bytes on rx08c-m/rx08c-w and repository all b'one\nsame line EDIT-W u3\nthree\n'; conflict list empty on both: True
- [17:16:11] u3 RESULT PASS

## u4: M loser: resolve --keep repository (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 16:04:53-16:26:16 IST)

- [16:10:39] u4: winner rx08c-w reached the repository 35 s after its edit
- [16:16:17] u4: winner W, loser M (rx08c-m). `conflict list` from M/W = {'M': [('rx08c-m', 'concurrent_update', '45d5b0948722')], 'W': [('rx08c-m', 'concurrent_update', '45d5b0948722')]}; `conflict show` revision from M/W = {'M': '45d5b0948722', 'W': '45d5b0948722'}. Listed once, same revision on both: True
- [16:18:05] u4: `conflict compare rx08c-m` run on rx08c-w: ok on try 7, machine version b'one\nsame line EDIT-M u4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W u4\nthree\n' == repository head True
- [16:18:30] u4: `conflict compare rx08c-m` run on rx08c-m: ok on try 2, machine version b'one\nsame line EDIT-M u4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W u4\nthree\n' == repository head True
- [16:19:14] u4: `conflict resolve rx08c-m --keep repository` run on rx08c-w -> queued; bytes on rx08c-m/rx08c-w and repository all b'one\nsame line EDIT-W u4\nthree\n'; conflict list empty on both: True
- [16:19:14] u4 RESULT PASS

## u2: M loser: resolve --keep machine (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 15:45:38-16:04:53 IST)

- [15:51:19] u2: winner rx08c-w reached the repository 23 s after its edit
- [15:52:50] u2: winner W, loser M (rx08c-m). `conflict list` from M/W = {'M': [('rx08c-m', 'merge_conflict', '54d1dbe8b0e1')], 'W': [('rx08c-m', 'merge_conflict', '54d1dbe8b0e1')]}; `conflict show` revision from M/W = {'M': '54d1dbe8b0e1', 'W': '54d1dbe8b0e1'}. Listed once, same revision on both: True
- [15:53:22] u2: `conflict compare rx08c-m` run on rx08c-w: ok on try 2, machine version b'one\nsame line EDIT-M u2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W u2\nthree\n' == repository head True
- [15:53:34] u2: `conflict compare rx08c-m` run on rx08c-m: ok on try 1, machine version b'one\nsame line EDIT-M u2\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-W u2\nthree\n' == repository head True
- [15:55:11] u2: `conflict resolve rx08c-m --keep machine` run on rx08c-w -> queued; bytes on rx08c-m/rx08c-w and repository all b'one\nsame line EDIT-M u2\nthree\n'; conflict list empty on both: True
- [15:55:11] u2 RESULT PASS

## v1: W loser: force pull, path scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 15:45:38-16:04:53 IST)

- [15:56:51] v1: winner rx08c-m reached the repository 35 s after its edit
- [16:03:35] v1: winner M loser W: `config force pull rx08c-w cfg/app/x08-v1.txt` run on rx08c-m -> queued; bytes on both and repository b'one\nsame line EDIT-M v1\nthree\n'; conflict list empty: True
- [16:03:35] v1 RESULT PASS

## p1: W loser: force push, path scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 16:04:53-16:26:16 IST)

- [16:21:00] p1: winner rx08c-m reached the repository 28 s after its edit
- [16:24:51] p1: winner M loser W: `config force push rx08c-w cfg/app/x08-p1.txt` run on rx08c-m -> queued; bytes on both and repository b'one\nsame line EDIT-W p1\nthree\n'; conflict list empty: True
- [16:24:51] p1 RESULT PASS

## p2: W loser: force pull, config scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 16:26:16-16:46:33 IST)

- [16:42:03] p2: winner rx08c-m reached the repository 53 s after its edit
- [16:45:19] p2: winner M loser W: `config force pull rx08c-w` run on rx08c-m -> queued; bytes on both and repository b'one\nsame line EDIT-M p2\nthree\n'; conflict list empty: True
- [16:45:19] p2 RESULT PASS

## v5: M loser: force pull, path scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 16:46:33-17:05:17 IST)

- [16:57:24] v5: winner rx08c-w reached the repository 35 s after its edit
- [17:03:56] v5: winner W loser M: `config force pull rx08c-m cfg/app/x08-v5.txt` run on rx08c-w -> queued; bytes on both and repository b'one\nsame line EDIT-W v5\nthree\n'; conflict list empty: True
- [17:03:56] v5 RESULT PASS

## v2: M loser: force push, path scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 16:26:16-16:46:33 IST)

- [16:32:11] v2: winner rx08c-w reached the repository 34 s after its edit
- [16:39:51] v2: winner W loser M: `config force push rx08c-m cfg/app/x08-v2.txt` run on rx08c-w -> queued; bytes on both and repository b'one\nsame line EDIT-M v2\nthree\n'; conflict list empty: True
- [16:39:51] v2 RESULT PASS

## v4: M loser: force pull, config scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold 17:05:17-17:22:35 IST)

- [17:19:00] v4: winner rx08c-w reached the repository 59 s after its edit
- [17:21:13] v4: winner W loser M: `config force pull rx08c-m` run on rx08c-w -> queued; bytes on both and repository b'one\nsame line EDIT-W v4\nthree\n'; conflict list empty: True
- [17:21:13] v4 RESULT PASS

## y6: M loser: force push, config scope (PASS)

Machines and `pb version`: rx08c-w2 dev-8a88ce3bc, rx08c-m dev-7432cbafe (2026-11-16, hold 17:49:09-18:11:58 IST)

- [18:06:18] y6: winner rx08c-w2 reached the repository 28 s after its edit
- [18:10:02] y6: winner W loser M: `config force push rx08c-m` run on rx08c-w2 -> queued; bytes on both and repository b'one\nsame line EDIT-M y6\nthree\n'; conflict list empty: True
- [18:10:02] y6 RESULT PASS

## a1: config approve run on the Mac, applied to Windows (exact head) (PASS)

Machines and `pb version`: rx08c-w2 dev-8a88ce3bc, rx08c-m dev-7432cbafe (2026-11-16, hold 18:11:58-18:24:20 IST)

- [18:20:04] a1: automatic updates off on rx08c-w2; edit on rx08c-m -> rx08c-w2 state pending error_code review_required review ['', '', '']; rx08c-w2 file unchanged before approval: True
- [18:21:11] a1: status before approve ('pending', 'review_required', '0486290f6c', '', 7); approve output {"assignment": {"configuration_revision": "35b7176b5130a26709383648f02f53894a065214c000cce774a4aef9e280c389", "path_rules": [{"source": "os", "id": "os:cfg/app"
- [18:21:35] a1: status after approve ('healthy', None, '0486290f6c', '0486290f6c', 8)
- [18:21:58] a1: status after approve ('healthy', None, '0486290f6c', '0486290f6c', 8)
- [18:22:21] a1: status after approve ('healthy', None, '0486290f6c', '0486290f6c', 8)
- [18:23:05] a1: `config approve rx08c-w2` run on rx08c-m -> {'assignment': {'configuration_revision': '35b7176b5130a26709383648f02f53894a065214c000cce774a4aef9e280c389', 'path_rules': [{'source': 'os', 'id': 'os:cfg/app', 'repository_path': 'cfg/app', 'local_path': '%APPDATA%/xplat/app', 'kind': 'directory', 'include': ['**/*.json', '**/*.txt'], 'exclude': ['**/*.tmp']}, {'source': 'os', 'id': 'os:cfg/settings.json', 'repository_path': 'cfg/settings.json', 'local_path': '%APPDATA%/xplat/settings.json', 'kind': 'file'}], 'repository_bindings': [{'repository_id': 'repository_a5db1169-cce7-43c9-9e92-63225b033276', 'url': 'http://127.0.0.1:24322/pair.git'}], 'id': 'assignment_00e818f4-e06c-4492-94d4-6b73c400c760', 'machine_id': 'machine_17e129aa-72c6-4c23-b0ab-2d38ec86b366', 'environment_id': 'machine_17e129aa-72c6-4c23-b0ab-2d38ec86b366', 'pull_repository_id': 'repository_a5db1169-cce7-43c9-9e92-63225b
- [18:23:06] a1 RESULT PASS

## a2: config approve run on Windows, applied to the Mac (exact head) (PASS)

Machines and `pb version`: rx08c-w2 dev-8a88ce3bc, rx08c-m dev-7432cbafe (2026-11-16, hold 18:24:20-18:35:29 IST)

- [18:31:31] a2: automatic updates off on rx08c-m; edit on rx08c-w2 -> rx08c-m state pending error_code review_required review ['', '', '']; rx08c-m file unchanged before approval: True
- [18:32:40] a2: status before approve ('pending', 'review_required', '28b30b0c11', '', 42); approve output {"assignment": {"configuration_revision": "01e62a4d0c90301e31d501032feed24dae2433421047480b75d46b4cdf40fc17", "path_rules": [{"source": "os", "id": "os:cfg/app"
- [18:33:04] a2: status after approve ('healthy', None, '28b30b0c11', '28b30b0c11', 43)
- [18:33:28] a2: status after approve ('healthy', None, '28b30b0c11', '28b30b0c11', 43)
- [18:33:52] a2: status after approve ('healthy', None, '28b30b0c11', '28b30b0c11', 43)
- [18:34:16] a2: `config approve rx08c-m` run on rx08c-w2 -> {'assignment': {'configuration_revision': '01e62a4d0c90301e31d501032feed24dae2433421047480b75d46b4cdf40fc17', 'path_rules': [{'source': 'os', 'id': 'os:cfg/app', 'repository_path': 'cfg/app', 'local_path': '~/Library/Application Support/xplat/app', 'kind': 'directory', 'include': ['**/*.json', '**/*.txt'], 'exclude': ['**/*.tmp']}, {'source': 'os', 'id': 'os:cfg/settings.json', 'repository_path': 'cfg/settings.json', 'local_path': '~/Library/Application Support/xplat/settings.json', 'kind': 'file'}], 'repository_bindings': [{'repository_id': 'repository_94d276fe-b67a-4bc1-b2b2-a4142fef4e4c', 'url': 'http://127.0.0.1:25496/pair.git'}], 'id': 'assignment_a5875b18-990e-42a4-b3a8-630629294873', 'machine_id': 'machine_4e309302-c920-4012-8863-c19bd6ff55e5', 'environment_id': 'machine_4e309302-c920-4012-8863-c19bd6ff55e5', 'pull_repository_id': 'r
- [18:34:16] a2 RESULT PASS

## v3: W loser: force push, config scope (PASS)

Machines and `pb version`: rx08c-w dev-ce199c78d, rx08c-m dev-7432cbafe (2026-11-16, hold from 15:20 IST; another hold of mine ran on the same two machines in parallel for part of it, which can only break a flow, never fake the exact-bytes result below; a clean rerun was attempted after the runners expired and could not be completed because their slots were taken over by other lanes)

- [15:34:46] v3: winner rx08c-m reached the repository 40 s after its edit
- [15:42:22] v3: winner M loser W: `config force push rx08c-w` run on rx08c-m -> queued; bytes on both and repository b'one\nsame line EDIT-W v3\nthree\n'; conflict list empty: True
- [15:42:22] v3 RESULT PASS

## Not run on a runner I still owned

No second v3 pass: the slots macpool-4 and winpool-1 passed to other lanes (R-X-41-N, fix-pathloss2) after my runners expired at about 18:42; my later cleanup commands touched those machines (config-sync.toml removed, two repository credentials set and removed), reported to the coordinator.

