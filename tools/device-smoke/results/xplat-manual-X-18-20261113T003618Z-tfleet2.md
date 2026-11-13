# T-FLEET2: X-18 L<->M on main dev-94eb504aa (includes 6ffa76bdb), 2026-11-13 05:41-06:05 IST

`pb version` lanex2-l and lanex2-m: dev-94eb504aa (W not in this check; L<->M pair only).

Killed-pb child on the Mac (fix 6ffa76bdb): `pb ssh lanex2-l -- sleep 120` in the background, wait 6 s, list the direct children of pb
(`pgrep -P`), `kill -KILL <pb>`, wait 4 s, `ps -p <child>`.
- run 1: pb 897, children 899 (/usr/bin/ssh) and 900 (/bin/sh watcher); both gone after the kill: PASS
- run 2: pb 1031, children 1033, 1034; both gone: PASS
- run 3: pb 1169, children 1176, 1178; both gone: PASS (before the fix, dev-6aa924079: the ssh child stayed with PPID 1, 3/3)

200 MiB tree (`nested/big.bin`, SHA256 7f7d58615b98b59a541134d765b9d2aee64c3f2354a3b7087aa31c0505586108):
- `pb scp -r <tree> lanex2-m:/Users/admin/...` from L: runs 1-3 exit 0, 115 s / 82 s / 73 s, SHA256 equal: PASS 3/3
- `pb scp -r /Users/admin/<tree> lanex2-l:/home/tester/...` from M: first try runs 1-2 hit the harness 120 s limit (partial files, not a pb error),
  run 3 exit 0 (90 s). Redone with a 600 s limit: run 1 FAIL rc=1 after 34 s, run 2 exit 0 (103 s), run 3 exit 0 (70 s).
  Third batch: runs 1-3 exit 0 (64-66 s, SHA256 equal), run 4 FAIL rc=1 after 22 s (06:04:11 IST).
  Both failures print: `pb: Paperboat could not finish the command ... Support reference: support_80ab8b12-2fe4-4fcb-9d92-04c60135b038`
  then `Connection to lanex2-l.ppr.boats closed by remote host. client_loop: send disconnect: Broken pipe. lost connection`.
  M>L: 7 of 9 runs passed. L>M: 3/3. The L daemon diagnostics hold no event at the failure second (only refresh_answered, pto); the Mac
  diagnostics for that window hold no event under that support reference.
- Haiku code read (no runtime proof): most likely an admitted scope dropped on a signed-config refresh mid-stream
  (cli/internal/peertransport/native/scopes.go:85-90, tailnet/authority.go:728-745), surfaced as a typed-less net.ErrClosed/ErrAdmission
  and printed as the generic message (cli/cmd/pb/command_failure_message.go:130). Not verified.

Result: the killed-pb part passes; M>L scp -r of a large tree fails intermittently (about 2 in 9). X-18 L<->M stays [~].
Scratch trees and the Mac/L test files were removed.
