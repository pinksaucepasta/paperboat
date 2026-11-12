# X-24 fixes by hand, lane FIX-T (fixt-l, fixt-l2 = Linux containers on hp; fixt-w = winpool runner), 2026-11-13 IST

Builds under test: dev-c0609dd16 (keys before attach), dev-4bc02ace0 (status bar), dev-c9b2c853d (Ctrl-C); `pb version` read on every machine after each install.
Method: a pty on the client machine runs `pb connect <target> new --name ...`; keys are typed blind; the bytes the client terminal received are counted.

| Part | Before (dev-be7f349ca, failures.md) | After |
|---|---|---|
| Linux client -> Windows target: 12 numbered `echo` lines typed from +1 s while the connection is made | 7 of 8 and 13 of 14 lines merged into one command (Enter lost) | all 12 ran as separate commands (dev-c0609dd16, c0609dd16 `HoldInput`) |
| Windows client (ssh -tt, 30x100): `pb connect <linux> --status-bar on`, `stty size` on the target | target 30 100, no bar row | target `29 100`, bar row drawn (dev-4bc02ace0) |
| Ctrl-C under `yes` (PRE 8 s), Linux client -> Linux target, painted after Ctrl-C | 60 s cap, 17-19 MB (dev-e265c5992 and older) | 17-21 s, 7.5-8.6 MB (dev-c9b2c853d): host cuts in 134 ms (discarded=true), 5.9 MB held and dropped; the remaining ~8 MB is queued downstream of the cut, not found. NOT under 1 s: cell stays [~] |

Not changed: 256 KiB client queues (tried, 17 s unchanged, reverted).
