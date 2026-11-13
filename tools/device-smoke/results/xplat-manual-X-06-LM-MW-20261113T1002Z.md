# R-X-06: X-06 L<->M and M<->W on the shared fleet, build dev-cf0c0a620 (has fix b71c48700), 2026-11-13 ~15:20-15:35 IST

Machines: lanex2-l (Linux container on hp), lanex2-m (macOS, the pbmac VM `admin` user), lanex2-w (real Victus Windows 11). `pb version` of all three read right before the run: dev-cf0c0a620.
Each run under flock /home/ubuntu/pb-wt/fleet-configsync.lock; config-sync.toml of each machine backed up and restored byte for byte (restore identical on L, M, W). Scratch only: repository laner-x06-config-fixture (hp, removed; fleet repository list shows none left), app dir laner-x06-app.

## M<->W: `xplat_config.py --only X-06 --devices M,W`, results/xplat-config-20261113T095250Z.json, 6/6 PASS
- x06 M>W, W>M: rules with repository paths differing only by case refused; destinations differing only by case refused; 10 reserved/unsafe repository names (CON, prn, Aux.log, NUL, com1.txt, LPT9, a:b.txt, trail., "trail ", dir./f.txt) refused; plain rule accepted (control), on lanex2-m and lanex2-w.
- x06_case_collision M>W, W>M: `Case-*.txt` and `case-*.txt` in a directory rule reported as case_collision on both machines and created on neither.
- x06_windows_case M>W, W>M: on lanex2-w two destinations differing only by case (`C:\Users\X\...\App.json` / `c:\users\x\...\APP.json`) refused as one path.

## L<->M: `xplat_config.py --only X-06 --devices L,M`, results/xplat-config-20261113T100140Z.json, 6/6 PASS
- x06 L>M, M>L and x06_case_collision L>M, M>L: same checks as above on lanex2-l and lanex2-m (the Mac's case-insensitive volume reports the collision from the repository commit; before b71c48700 it could not). x06_windows_case: n/a, no Windows device in this pair (the Windows part is in the L<->W and M<->W cells).

L<->W: see xplat-manual-X-06-LW-20261113T0816Z.md (dev-b71c48700, own Linux container + Windows runner, 6/6).
