# X-08 L<->M, Linux as loser, shared fleet (lanex2-l/-m), 2026-11-13 IST 17:55-18:52 (PARTIAL, not ticked)

Fleet `pb version`: lanex2-l and lanex2-m dev-79203d6e5 (includes cec6abcc4); lanex2-w still dev-cf0c0a620 (Windows install pending, FLEET3), so M<->W was not run. Under fleet-configsync.lock, config-sync.toml restored byte-for-byte after each hold, scratch repository deleted.
Method: to make Linux lose the race, SIGSTOP the Linux sync worker (`pb daemon __runtime-worker`, pid on lanex2-l) before the Mac edits, wait until the Mac's edit is in the repository, write the Linux edit, SIGCONT. (Stopping the main `pb daemon --config` process instead did not stop sync.)

## PASS (Linux loser)

- y1: winner lanex2-m reached the repository 13 s after its edit
- y1 dump repo: 'b0f3b99 12:48:35 Synchronize configuration\n0b16f19 12:47:30 Synchronize configuration\none\nsame line EDIT-M y1\nthree\n'
- y1: winner M, loser L (lanex2-l). `conflict list` from L/M = {'L': [('lanex2-l', 'merge_conflict', '0ae315fd5b68')], 'M': [('lanex2-l', 'merge_conflict', '0ae315fd5b68')]}; `conflict show` revision from L/M = {'L': '0ae315fd5b68', 'M': '0ae315fd5b68'}. Listed once, same revision on both: True
- y1: `conflict compare lanex2-l` run on lanex2-m: ok on try 2, machine version b'one\nsame line EDIT-L y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M y1\nthree\n' == repository head True
- y1: `conflict compare lanex2-l` run on lanex2-l: ok on try 1, machine version b'one\nsame line EDIT-L y1\nthree\n' == loser bytes True, repository version b'one\nsame line EDIT-M y1\nthree\n' == repository head True
- y1: `conflict resolve lanex2-l --keep repository` run on lanex2-m -> queued; bytes on lanex2-l/lanex2-m and repository all b'one\nsame line EDIT-M y1\nthree\n'; conflict list empty on both: True
- y1 RESULT PASS

## Not passing / not seen
- w4 (`resolve --keep machine`, Linux loser): first run the conflict was held by the Mac as `concurrent_update` (list once/same revision: true) and vanished before compare/resolve ("has no configuration conflict"); the freeze did not make Linux the loser that time (same flow passed as y1 once, so a race, not a deterministic result). The following force flows then timed out waiting for the Mac's edit to reach the repository (environment left in conflict state).
- force pull / force push (path and env scope) with Linux loser, and `config approve` L<->M: not run. hp (host of lanex2-l and of the repository) went offline at ~19:15 IST; the Mac VM also rebooted once at ~18:12 IST during the runs.
