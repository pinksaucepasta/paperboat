# X-03 L<->M after the pull-half fix (lane R-X-03B), 2026-11-13 ~18:00-19:20 IST, NOT ticked

Fix: 9e9447e60 (pull half treats a local file equal to the push half's last published state as unchanged; test TestPullHalfTreatsWhatThePushHalfPublishedAsUnchanged, PASS lines in CI run 43421361080).
Machines: laner-x03b-l (Linux container on hp), laner-x03b-m (hosted macOS runner macpool-3). `pb version` on both: dev-9e9447e60.
Command: `python3 tools/device-smoke/xplat_config.py --only X-03 --devices L,M` (results/xplat-config-20261113T122404Z.json) and two reruns of x03_merge alone.

- PASS X-03:x03_bytes:L>M 103s, X-03:x03_bytes:M>L 19s
- PASS X-03:x03_merge:L>M 217s three-way merge kept CRLF on every line (this failed 5 of 5 before the fix)
- FAIL X-03:x03_merge:M>L 725s, again 735 s and 733 s on two reruns: M keeps 'A1' (state=conflict), L keeps 'B5' (conflict concurrent_update), no merge on either side
- PASS X-03:x03_eol_edit:L>M 148s, X-03:x03_eol_edit:M>L 139s
Not diagnosed: a Haiku read of the code could not reproduce the stuck state in a two-machine model (both edit orders converge); the four baseline.json files and the repository log at the stuck moment are needed. The next attempt was stopped when hp went offline (Tailscale) and the macpool-3 Mac was recycled by another lane.
