# R-X-07: X-07 L<->W on cycle 1 build dev-aee62911e, 2026-11-13 ~07:00-07:15 IST

Machines (own, nothing shared): laner07-l (Linux container laner-x07-l on hp, machine_21235538) and laner07-w (hosted Windows Server runner winpool-5, machine_aedb9a1c).
`pb version`: laner07-l dev-aee62911e, laner07-w dev-aee62911e (both installed from /home/ubuntu/pb-builds/dev-aee62911e).
Run: tools/device-smoke/xplat_config.py --only X-07 --devices L,W (scratch repository laner07-config-fixture, app dir laner07-app), result results/xplat-config-20261113T012641Z.json, 6/6 PASS, both directions:

- x07_unicode L>W, W>L: a name written decomposed (NFD "cafe\u0301-<tag>.txt") plus a CJK/umlaut name arrive on the peer; one file per name (NFC-compared) on both devices, an edit on the peer round-trips, no conflict in `pb config status`.
- x07_long_path L>W, W>L: a 388-character destination path on Windows (6 nested directories of 47 characters) arrives byte-identical.
- x07_paths L>W, W>L: a drive-letter `local_path` (`C:\Users\...\drive.txt`) is accepted on Windows (`validate`), and refused on Linux with exit 1, nothing created (no literal `C:\Users...` file under the home directory).

## Part not passing (by hand on laner07-l, same fixture): the refusal is not a clear message, and the backslash form is accepted on Unix
- `local_path = "C:\\Users\\someone\\x07\\drive.txt"` on Linux: `pb config sync validate` rc 1 with only "Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` ..." (--json: code operation_failed, category local_io). No mention of the path or the OS.
- `local_path = "~\\x07\\back.txt"` on Linux: `validate` rc 0, the rule is listed with the raw backslash path, i.e. it would be applied as a file literally named `x07\back.txt` under the home directory.
The row asks for a clear message and for no literal path, so L<->W stays [~] until both are fixed and re-run on the next cycle.
