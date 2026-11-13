# T-FLEET2: X-18 L<->M on cycle 2 dev-e4d2cc0f8 (main with 6ffa76bdb), 2026-11-13 08:02-08:21 IST

lanex2-l (Linux container) and lanex2-m (macOS, pbmac VM) both `pb version` dev-e4d2cc0f8, doctor healthy; W was not in this pair.
The Mac was re-installed and re-paired at 08:01 IST (fleet restore), so these are the first runs on the clean install.

versions dev-e4d2cc0f8 dev-e4d2cc0f8
run 1 pb=53273 children=[53275 53276 ]
  child 53275 gone
  child 53276 gone
run 2 pb=53419 children=[53421 53422 ]
  child 53421 gone
  child 53422 gone
run 3 pb=53625 children=[53627 53628 ]
  child 53627 gone
  child 53628 gone

L>M run 1 08:02:31: rc=0 251s equal=True 
L>M run 2 08:06:43: rc=0 84s equal=True 
L>M run 3 08:08:07: rc=0 90s equal=True 
M>L run 1 08:09:37: rc=0 74s equal=True 
M>L run 2 08:10:52: rc=0 96s equal=True 
M>L run 3 08:12:28: rc=0 79s equal=True 
M>L run 4 08:13:48: rc=0 101s equal=True 
M>L run 5 08:15:29: rc=0 91s equal=True 
M>L run 6 08:17:03: rc=0 75s equal=True 
M>L run 7 08:18:19: rc=0 77s equal=True 
M>L run 8 08:19:36: rc=0 70s equal=True 
M>L failures 0 DONE 08:20:47

## Result
- killed-pb child on the Mac (fix 6ffa76bdb): 3 of 3 runs, `pb ssh` killed with SIGKILL, both children (ssh and the watcher shell) gone 4 s later.
- `pb scp -r` of a 200 MiB tree, SHA256 equal: L>M 3 of 3 (251 s for the first cold run, then 84 s and 90 s), M>L 8 of 8 here and 3 of 3 in the first set (70-101 s each), no failure.
- Earlier today on dev-94eb504aa two M>L runs of 9 failed (rc=1, 'Paperboat could not finish the command', Broken pipe, results/xplat-manual-X-18-20261113T0*-tfleet2.md). One coincided with another lane's staging install on the Mac VM (the Mac daemon restarted 24 s later); the other has no matching restart or error on L. Not reproduced in 11 consecutive M>L runs on cycle 2 with the Mac left alone, and L<->W 10 of 10 on dev-94eb504aa; not marked fixed, no code change.
- Scratch trees removed on both machines.
