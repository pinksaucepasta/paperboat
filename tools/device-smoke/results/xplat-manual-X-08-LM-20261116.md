# X-08 L<->M: conflicts across Linux and macOS, by hand on the shared fleet lanex2-l2 (Linux container on anvit, arm64) and lanex2-m (macOS VM), plus one own macOS runner for the last part, 2026-11-14..16 IST (R-X-08C)

Harness: the predecessor driver (scratch f2.py/app2.py/setup2.py): a scratch repository on anvit, one config-sync assignment per device (cfg/app/*.txt), the same line edited on both devices; every flow runs a hold of at most 15 min under fleet-mac.lock then fleet-configsync.lock (or only the config-sync lock when the Mac is the own macpool runner), config-sync.toml restored and the assignment and repository removed after each hold. To make Linux the loser the Linux sync worker (`pb daemon __runtime-worker`) is SIGSTOPped until the Mac edit is in the repository, then SIGCONTed; the Mac loses without a freeze. Each flow: `conflict list` from both devices (once, same revision), `conflict show`, `conflict compare` from both (byte-exact), then the resolution or force command from the other device, equal bytes on both devices and the repository, conflict list empty. Per the 2026-11-16 speed rules the parts passed on different builds (config sync code on the two sides did not change between them: `git log cec6abcc4.. -- cli/internal/hostruntime/configsync` shows only the Windows owner-rule commit f44ad1a30).

## w4: Linux loser: resolve --keep machine (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 03:31:05 IST)

- [03:28:12] w4: winner lanex2-m reached the repository 12 s after its edit
- [03:29:40] w4: winner M, loser L (lanex2-l2). `conflict list` from L/M = {'L': [('lanex2-l2', 'merge_conflict', '72e9e6d1fa54')], 'M': [('lanex2-l2', 'merge_conflict', '72e9e6d1fa54')]}; `conflict show` revision from L/M = {'L': '72e9e6d1fa54', 'M': '72e9e6d1fa54'}. Listed once, same revision on both: True
- [03:29:49] w4: `conflict compare lanex2-l2` run on lanex2-m: ok on try 2, machine version b'one\nsame line EDIT-L w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M w4\nthree\n' == repository head True
- [03:29:53] w4: `conflict compare lanex2-l2` run on lanex2-l2: ok on try 1, machine version b'one\nsame line EDIT-L w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M w4\nthree\n' == repository head True
- [03:31:04] w4: `conflict resolve lanex2-l2 --keep machine` run on lanex2-m -> queued; bytes on lanex2-l2/lanex2-m and repository all b'one\nsame line EDIT-L w4\nthree\n'; conflict list empty on both: True
- [03:31:04] w4 RESULT PASS
- [03:32:45] w4: winner lanex2-m reached the repository 13 s after its edit
- [03:34:18] w4: winner M, loser L (lanex2-l2). `conflict list` from L/M = {'L': [('lanex2-l2', 'merge_conflict', '1d2a182efe5a')], 'M': [('lanex2-l2', 'merge_conflict', '1d2a182efe5a')]}; `conflict show` revision from L/M = {'L': '1d2a182efe5a', 'M': '1d2a182efe5a'}. Listed once, same revision on both: True
- [03:34:23] w4: `conflict compare lanex2-l2` run on lanex2-m: ok on try 1, machine version b'one\nsame line EDIT-L w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M w4\nthree\n' == repository head True
- [03:34:26] w4: `conflict compare lanex2-l2` run on lanex2-l2: ok on try 1, machine version b'one\nsame line EDIT-L w4\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M w4\nthree\n' == repository head True
- [03:35:33] w4: `conflict resolve lanex2-l2 --keep machine` run on lanex2-m -> queued; bytes on lanex2-l2/lanex2-m and repository all b'one\nsame line EDIT-L w4\nthree\n'; conflict list empty on both: True
- [03:35:33] w4 RESULT PASS

## y1: Linux loser: resolve --keep repository (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 03:31:05 IST)

- [03:35:46] y1: winner lanex2-m reached the repository 13 s after its edit
- [03:36:56] y1: winner M, loser L (lanex2-l2). `conflict list` from L/M = {'L': [('lanex2-l2', 'merge_conflict', '0863684129bb')], 'M': [('lanex2-l2', 'merge_conflict', '0863684129bb')]}; `conflict show` revision from L/M = {'L': '0863684129bb', 'M': '0863684129bb'}. Listed once, same revision on both: True
- [03:37:00] y1: `conflict compare lanex2-l2` run on lanex2-m: ok on try 1, machine version b'one\nsame line EDIT-L y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M y1\nthree\n' == repository head True
- [03:37:04] y1: `conflict compare lanex2-l2` run on lanex2-l2: ok on try 1, machine version b'one\nsame line EDIT-L y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M y1\nthree\n' == repository head True
- [03:37:36] y1: `conflict resolve lanex2-l2 --keep repository` run on lanex2-m -> queued; bytes on lanex2-l2/lanex2-m and repository all b'one\nsame line EDIT-M y1\nthree\n'; conflict list empty on both: True
- [03:37:36] y1 RESULT PASS
- [04:51:41] y1: winner lanex2-m reached the repository 19 s after its edit
- [04:52:58] y1: winner M, loser L (lanex2-l2). `conflict list` from L/M = {'L': [('lanex2-l2', 'merge_conflict', 'e921e055370c')], 'M': [('lanex2-l2', 'merge_conflict', 'e921e055370c')]}; `conflict show` revision from L/M = {'L': 'e921e055370c', 'M': 'e921e055370c'}. Listed once, same revision on both: True
- [04:53:26] y1: `conflict compare lanex2-l2` run on lanex2-m: ok on try 3, machine version b'one\nsame line EDIT-L y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M y1\nthree\n' == repository head True
- [04:53:33] y1: `conflict compare lanex2-l2` run on lanex2-l2: ok on try 1, machine version b'one\nsame line EDIT-L y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M y1\nthree\n' == repository head True
- [04:54:23] y1: `conflict resolve lanex2-l2 --keep repository` run on lanex2-m -> queued; bytes on lanex2-l2/lanex2-m and repository all b'one\nsame line EDIT-M y1\nthree\n'; conflict list empty on both: True
- [04:54:23] y1 RESULT PASS

## y2: Linux loser: force pull, path scope (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-14, hold start 06:56:26 IST)

- [06:58:03] y2: winner lanex2-m reached the repository 12 s after its edit
- [06:59:47] y2: winner M loser L: `config force pull lanex2-l2 cfg/app/x08-y2.txt` run on lanex2-m -> queued; bytes on both and repository b'one\nsame line EDIT-M y2\nthree\n'; conflict list empty: True
- [06:59:47] y2 RESULT PASS

## y3: Linux loser: force push, path scope (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-14, hold start 06:56:26 IST)

- [07:01:02] y3: winner lanex2-m reached the repository 12 s after its edit
- [07:03:53] y3: winner M loser L: `config force push lanex2-l2 cfg/app/x08-y3.txt` run on lanex2-m -> queued; bytes on both and repository b'one\nsame line EDIT-L y3\nthree\n'; conflict list empty: True
- [07:03:53] y3 RESULT PASS

## y5: Linux loser: force pull, config (env) scope (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 03:47:54 IST)

- [03:49:36] y5: winner lanex2-m reached the repository 13 s after its edit
- [03:51:26] y5: winner M loser L: `config force pull lanex2-l2` run on lanex2-m -> queued; bytes on both and repository b'one\nsame line EDIT-M y5\nthree\n'; conflict list empty: True
- [03:51:26] y5 RESULT PASS

## y6: Linux loser: force push, config (env) scope (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 03:47:54 IST)

- [03:52:32] y6: winner lanex2-m reached the repository 7 s after its edit
- [03:55:26] y6: winner M loser L: `config force push lanex2-l2` run on lanex2-m -> queued; bytes on both and repository b'one\nsame line EDIT-L y6\nthree\n'; conflict list empty: True
- [03:55:26] y6 RESULT PASS

## a2: Linux loser: config approve run on the Mac (exact head applied to Linux) (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 04:12:11 IST)

- [04:15:24] a2: automatic updates off on lanex2-l2; edit on lanex2-m -> lanex2-l2 state pending error_code review_required review ['', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '']; lanex2-l2 file unchanged before approval: True
- [04:16:26] a2: status before approve ('pending', 'review_required', '81ab5ffcbc', '', 138); approve output {"assignment": {"configuration_revision": "fe563521421ac0b315bce352c32ee21cdbb6288817f0430497a5cd425309ba26", "path_rules": [{"source": "os", "id": "os:cfg/app"
- [04:16:47] a2: status after approve ('pending', 'review_required', '81ab5ffcbc', '', 139)
- [04:17:07] a2: status after approve ('healthy', None, '81ab5ffcbc', '81ab5ffcbc', 139)
- [04:17:29] a2: status after approve ('healthy', None, '81ab5ffcbc', '81ab5ffcbc', 139)
- [04:17:34] a2: `config approve lanex2-l2` run on lanex2-m -> {'assignment': {'configuration_revision': 'fe563521421ac0b315bce352c32ee21cdbb6288817f0430497a5cd425309ba26', 'path_rules': [{'source': 'os', 'id': 'os:cfg/app', 'repository_path': 'cfg/app', 'local_path': '~/.config/xplat/app', 'kind': 'directory', 'include': ['**/*.json', '**/*.txt'], 'exclude': ['**/*.tmp']}, {'source': 'os', 'id': 'os:cfg/settings.json', 'repository_path': 'cfg/settings.json', 'local_path': '~/.config/xplat/settings.json', 'kind': 'file'}], 'repository_bindings': [{'repository_id': 'repository_169b7c2a-a376-434b-8844-0e5a7d82f148', 'url': 'http://172.17.0.1:21178/pair.git'}], 'id': 'assignment_5a186127-377e-4442-bf2b-0d467a81406a', 'machine_id': 'machine_8139eee2-c661-484a-b1de-43be3f25e126', 'environment_id': 'machine_8139eee2-c661-484a-b1de-43be3f25e126', 'pull_repository_id': 'repository_169b7c2a-a376-43
- [04:17:34] a2 RESULT PASS

## a1: Mac loser: config approve run on Linux (exact head applied to the Mac) (PASS)

Machines and `pb version`: lanex2-l2 dev-a2742604e, lanex2-m dev-729ce3c0e (2026-11-16, hold start 09:57:12 IST)

- [04:41:20] a1 RESULT FAIL AssertionError('timed out waiting for base')
- [07:20:01] a1 RESULT FAIL AssertionError('timed out waiting for edit in repository')
- [09:39:41] a1: automatic updates off on lanex2-m; edit on lanex2-l2 -> lanex2-m state pending error_code review_required review ['', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '']; lanex2-m file unchanged before approval: True
- [09:40:51] a1 RESULT FAIL RuntimeError('lanex2-l2: pb config approve lanex2-m failed: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues.')
- [10:03:17] a1: automatic updates off on lanex2-m; edit on lanex2-l2 -> lanex2-m state pending error_code review_required review ['', '', '', '', '', '', '']; lanex2-m file unchanged before approval: True
- [10:04:39] a1: status before approve ('pending', 'review_required', 'ca9b89bcef', '', 304); approve output {"assignment": {"configuration_revision": "caafe419e13235109250f14216233dc4001d13b206449c3633e794e768878c72", "path_rules": [{"source": "os", "id": "os:cfg/app"
- [10:05:04] a1: status after approve ('syncing', None, '', '', 305)
- [10:05:30] a1: status after approve ('healthy', None, 'ca9b89bcef', 'ca9b89bcef', 305)
- [10:05:52] a1: status after approve ('healthy', None, 'ca9b89bcef', 'ca9b89bcef', 305)
- [10:06:59] a1: `config approve lanex2-m` run on lanex2-l2 -> {'assignment': {'configuration_revision': 'caafe419e13235109250f14216233dc4001d13b206449c3633e794e768878c72', 'path_rules': [{'source': 'os', 'id': 'os:cfg/app', 'repository_path': 'cfg/app', 'local_path': '~/Library/Application Support/xplat/app', 'kind': 'directory', 'include': ['**/*.json', '**/*.txt'], 'exclude': ['**/*.tmp']}, {'source': 'os', 'id': 'os:cfg/settings.json', 'repository_path': 'cfg/settings.json', 'local_path': '~/Library/Application Support/xplat/settings.json', 'kind': 'file'}], 'repository_bindings': [{'repository_id': 'repository_e2feb655-88b2-4bf2-8bb9-849a1b166fee', 'url': 'http://127.0.0.1:27904/pair.git'}], 'id': 'assignment_c88e160d-b275-434b-a12c-60c7d3ea04d4', 'machine_id': 'machine_9e1851b7-5763-40ab-9834-720becc3eb6d', 'environment_id': 'machine_9e1851b7-5763-40ab-9834-720becc3eb6d', 'pull_repos
- [10:06:59] a1 RESULT PASS

## u3: Mac loser: conflict list/show/compare + resolve --keep machine (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 06:50:26 IST)

- [05:14:59] u3: winner lanex2-l2 reached the repository 51 s after its edit
- [06:54:08] u3: winner lanex2-l2 reached the repository 52 s after its edit
- [06:57:52] u3: winner L, loser M (lanex2-m). `conflict list` from L/M = {'L': [('lanex2-m', 'concurrent_update', '79ed129f2086')], 'M': [('lanex2-m', 'concurrent_update', '79ed129f2086')]}; `conflict show` revision from L/M = {'L': '79ed129f2086', 'M': '79ed129f2086'}. Listed once, same revision on both: True
- [06:58:25] u3: `conflict compare lanex2-m` run on lanex2-l2: ok on try 5, machine version b'one\nsame line EDIT-M u3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u3\nthree\n' == repository head True
- [06:58:30] u3: `conflict compare lanex2-m` run on lanex2-m: ok on try 1, machine version b'one\nsame line EDIT-M u3\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-L u3\nthree\n' == repository head True
- [07:00:21] u3: `conflict resolve lanex2-m --keep machine` run on lanex2-l2 -> queued; bytes on lanex2-l2/lanex2-m and repository all b'one\nsame line EDIT-M u3\nthree\n'; conflict list empty on both: True
- [07:00:21] u3 RESULT PASS
- [10:18:11] u3: winner lanex2-m reached the repository 22 s after its edit
- [10:18:38] u3 dump repo: '9345007 04:48:04 Synchronize configuration\n149eb59 04:47:03 Synchronize configuration\none\nsame line EDIT-M u3\nthree\n'
- [10:18:40] u3 dump status lanex2-m: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', '93450072'), (None, 'syncing', None, '149eb590', '149eb590'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-M u3\nthree\n'
- [10:18:48] u3 dump status lanex2-w: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', '93450072'), (None, 'conflict', 'config_conflict', '149eb590', '93450072'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-W u3\nthree\n'

## u1: Mac loser: resolve --keep repository (PASS)

Machines and `pb version`: dev-e4515714f (lanex2-l2 and lanex2-m, 2026-11-14 hold 01:01 IST)

- [05:12:59] u1 RESULT FAIL AssertionError('timed out waiting for base u1 on both')
- [06:10:51] u1 RESULT FAIL AssertionError('timed out waiting for winner in repository')
- [08:59:42] u1: winner lanex2-m reached the repository 39 s after its edit
- [09:07:52] u1 RESULT FAIL AssertionError('timed out waiting for a conflict on cfg/app/x08-u1.txt')
- [10:12:01] u1: winner lanex2-m reached the repository 13 s after its edit
- [10:12:34] u1 dump repo: '7261b69 04:41:57 Synchronize configuration\n0241ac5 04:40:35 Synchronize configuration\none\nsame line EDIT-M u1\nthree\n'
- [10:12:35] u1 dump status lanex2-m: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', '7261b69a'), (None, 'healthy', None, '0241ac5c', '0241ac5c'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-M u1\nthree\n'
- [10:12:45] u1 dump status lanex2-w: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', '7261b69a'), (None, 'syncing', None, '0241ac5c', '0241ac5c'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-W u1\nthree\n'
- [10:13:55] u1: winner M, loser W (lanex2-w). `conflict list` from M/W = {'M': [('lanex2-w', 'merge_conflict', '79cf915d2d25')], 'W': [('lanex2-w', 'merge_conflict', '79cf915d2d25')]}; `conflict show` revision from M/W = {'M': '79cf915d2d25', 'W': '79cf915d2d25'}. Listed once, same revision on both: True
- [10:14:30] u1: `conflict compare lanex2-w` run on lanex2-m: ok on try 3, machine version b'one\nsame line EDIT-W u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u1\nthree\n' == repository head True
- [10:15:14] u1: `conflict compare lanex2-w` run on lanex2-w: ok on try 3, machine version b'one\nsame line EDIT-W u1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M u1\nthree\n' == repository head True
- [10:16:08] u1: `conflict resolve lanex2-w --keep repository` run on lanex2-m -> queued; bytes on lanex2-m/lanex2-w and repository all b'one\nsame line EDIT-M u1\nthree\n'; conflict list empty on both: True
- [10:16:08] u1 RESULT PASS

## v1: Mac loser: force pull, path scope (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 05:33:35 IST)

- [05:27:44] v1 RESULT FAIL AssertionError('timed out waiting for base v1 on both')
- [05:37:10] v1: winner lanex2-l2 reached the repository 52 s after its edit
- [05:41:29] v1: winner L loser M: `config force pull lanex2-m cfg/app/x08-v1.txt` run on lanex2-l2 -> queued; bytes on both and repository b'one\nsame line EDIT-L v1\nthree\n'; conflict list empty: True
- [05:41:29] v1 RESULT PASS
- [09:32:19] v1 winner stall at 09:32:15: repo log '39eeffd 03:54:14 Synchronize configuration\na5d0f9e 03:52:02 Synchronize configuration\n9fe0e02 03:51:18 seed\n' repo file b'one\nsame line base v1\nthree\n' files {'lanex2-m': b'one\nsame line EDIT-M v1\nthree\n', 'lanex2-w': b'one\nsame line base v1\nthree\n'}
- [09:32:20] v1 winner stall status lanex2-m: p/x08-v3.txt: concurrent update; see both versions with `pb config conflict compare lanex2-m cfg/app/x08-v3.txt`, then keep one with `pb config conflict resolve lanex2-m cfg/app/x08-v3.txt --keep machine` or `--keep repository`
- [09:32:23] v1 winner stall status lanex2-w: p/x08-v3.txt: concurrent update; see both versions with `pb config conflict compare lanex2-m cfg/app/x08-v3.txt`, then keep one with `pb config conflict resolve lanex2-m cfg/app/x08-v3.txt --keep machine` or `--keep repository`
- [09:32:23] v1 RESULT FAIL AssertionError('timed out waiting for winner in repository')

## p1: Mac loser: force push, path scope (PASS)

Machines and `pb version`: lanex2-l2 dev-08d3c4462, lanex2-m dev-08d3c4462 (2026-11-16, hold start 05:45:42 IST)

- [05:29:46] p1: winner lanex2-l2 reached the repository 52 s after its edit
- [05:49:34] p1: winner lanex2-l2 reached the repository 39 s after its edit
- [05:54:34] p1: winner L loser M: `config force push lanex2-m cfg/app/x08-p1.txt` run on lanex2-l2 -> queued; bytes on both and repository b'one\nsame line EDIT-M p1\nthree\n'; conflict list empty: True
- [05:54:34] p1 RESULT PASS

## v3: Mac loser: force push, config scope (PASS)

Machines and `pb version`: lanex2-l2 dev-a2742604e, lanex2-m dev-432169341 (2026-11-16, hold start 09:08:54 IST)

- [09:11:38] v3: winner lanex2-l2 reached the repository 32 s after its edit
- [09:13:53] v3: winner L loser M: `config force push lanex2-m` run on lanex2-l2 -> queued; bytes on both and repository b'one\nsame line EDIT-M v3\nthree\n'; conflict list empty: True
- [09:13:53] v3 RESULT PASS

## p2: Mac loser: force pull, config scope (PASS)

Machines and `pb version`: lanex2-l2 dev-64faf9466, rx08c-m dev-7432cbafe (2026-11-16, hold start 14:43:56 IST)

- [09:56:13] p2 winner stall at 09:56:12: repo log 'ca67361 04:18:10 Synchronize configuration\nd57cc0f 04:16:58 Synchronize configuration\nd1af637 04:16:14 seed\n' repo file b'one\nsame line base p2\nthree\n' files {'lanex2-l2': b'one\nsame line base p2\nthree\n', 'lanex2-m': b'one\nsame line base p2\nthree\n'}
- [09:56:18] p2 winner stall status lanex2-l2:  pending clean
- [09:56:20] p2 winner stall status lanex2-m:  pending clean
- [09:56:20] p2 RESULT FAIL AssertionError('timed out waiting for winner in repository')
- [10:29:45] p2: winner lanex2-l2 reached the repository 46 s after its edit
- [10:30:10] p2 dump repo: 'cc71a71 04:59:41 Synchronize configuration\nd1bf837 04:58:41 Synchronize configuration\none\nsame line EDIT-L p2\nthree\n'
- [10:30:12] p2 dump status lanex2-l2: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', 'cc71a710'), (None, 'conflict', 'config_conflict', 'd1bf837f', 'cc71a710'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-L p2\nthree\n'
- [10:30:15] p2 dump status lanex2-m: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', 'cc71a710'), (None, 'conflict', 'config_conflict', 'd1bf837f', 'cc71a710'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-M p2\nthree\n'
- [10:34:07] p2 RESULT FAIL KeyError('conflicts')
- [14:47:32] p2: winner lanex2-l2 reached the repository 19 s after its edit
- [14:48:00] p2 dump repo: 'fb2dbb6 09:17:29 Synchronize configuration\n3d69ead 09:16:29 Synchronize configuration\none\nsame line EDIT-L p2\nthree\n'
- [14:48:01] p2 dump status lanex2-l2: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', 'fb2dbb6a'), (None, 'disabled', None, '', ''), (None, 'revoked', None, '', ''), (None, 'disabled', None, '', ''), (None, 'syncing', None, '3d69eadd', '3d69eadd'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-L p2\nthree\n'
- [14:48:10] p2 dump status rx08c-m: [(None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'conflict', 'config_conflict', '', 'fb2dbb6a'), (None, 'disabled', None, '', ''), (None, 'revoked', None, '', ''), (None, 'disabled', None, '', ''), (None, 'syncing', None, '3d69eadd', '3d69eadd'), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', ''), (None, 'disabled', None, '', '')] file=b'one\nsame line EDIT-M p2\nthree\n'
- [14:49:19] p2: winner L loser M: `config force pull rx08c-m` run on lanex2-l2 -> queued; bytes on both and repository b'one\nsame line EDIT-L p2\nthree\n'; conflict list empty: True
- [14:49:19] p2 RESULT PASS

