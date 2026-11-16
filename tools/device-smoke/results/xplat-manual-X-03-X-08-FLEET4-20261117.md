# X-03 M>L merge and X-08 first-file flow, rechecked on the fleet (lane FLEET4, 2026-11-17 00:57-01:18 IST)

Build dev-c7ed1040f on lanex2-l2 (Linux container on anvit), lanex2-m (the pbmac Tart VM) and lanex2-w, installed 00:53-00:55 IST over dev-6232eebec; explicit `pb daemon machine-guard install` on all three (doctor healthy, "machine guard runs this pb's version" on each, exec 6/6 in 0.9-2.3 s before the run). The build contains c7ed1040f (config sync: a file the other machine created and both machines edited at once now merges) and 55c2d21fe/8b653b1f0 (a file written during the first sync is published). Repository fixture: bare git + HTTP server on anvit (token), reached by lanex2-l2 over the docker bridge and by lanex2-m over the repository host's tailnet address; under fleet-configsync.lock and fleet-mac.lock.

## X-03 `x03_merge`, both directions (failures.md line 388: M>L stuck, never merged)
`xplat_config.py --only x03_merge --devices L,M` (CRLF file written on one side, settled in the repository, then both sides edited at once, line 1 on one side and line 5 on the other, no re-apply):
```
PASS X-03:x03_merge:L>M 176s three-way merge kept CRLF on every line
PASS X-03:x03_merge:M>L 131s three-way merge kept CRLF on every line
```
results/xplat-config-20261116T192733Z.json (this is the failing direction M>L, 131 s to the merged bytes on both sides, previously "3 of 3 never merged").

## X-08 first file after a fresh assignment, L -> M and the repository (failures.md line 455)
New check (scratch script, fresh repository + fresh assignment per cycle on lanex2-l2 and lanex2-m): variant A = a 300-byte file is written on L right after L's assignment and before M applies; variant B = right after both assignments. Pass = the file is in the repository and on M byte for byte within 240 s. 12 cycles (4 + 8, two holds):
```
== first08 19:33:12
01:03:39 cycle 0 (A): PASS repo +7s, M +12s
01:05:24 cycle 1 (B): PASS repo +60s, M +64s
01:05:59 cycle 2 (A): PASS repo +7s, M +11s
01:07:38 cycle 3 (B): PASS repo +59s, M +68s
01:07:47 FIRST08 PASS: 4 cycles, 0 failed
f08rc=0
== first08 round 2 19:38:19
01:08:47 cycle 0 (A): PASS repo +8s, M +12s
01:10:31 cycle 1 (B): PASS repo +59s, M +75s
01:11:14 cycle 2 (A): PASS repo +7s, M +11s
01:12:50 cycle 3 (B): PASS repo +59s, M +67s
01:13:31 cycle 4 (A): PASS repo +7s, M +12s
01:15:05 cycle 5 (B): PASS repo +59s, M +64s
01:15:42 cycle 6 (A): PASS repo +7s, M +11s
01:17:21 cycle 7 (B): PASS repo +59s, M +68s
01:17:38 FIRST08 PASS: 8 cycles, 0 failed
f08rc=0
```

All 12 cycles passed (variant A: repo +7-8 s, M +11-12 s; variant B: repo +59-60 s = one sync interval, M +64-75 s); the earlier failure was 2 of about 20 runs, so this is 12 clean runs on the fixed build, not a proof of absence.
