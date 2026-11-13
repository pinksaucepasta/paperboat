# X-03 M<->W (lane R-X-03B), 2026-11-13 ~18:00-18:20 IST, ticked

Machines (own trio, one account): laner-x03b-m (hosted macOS runner macpool-3, user runner), laner-x03b-w (hosted Windows runner winpool-5), laner-x03b-l (Linux container on hp, repository host).
`pb version` on all three: dev-9e9447e60 (main 9e9447e60, includes the pull-half fix; install receipts of `pb install --json`).
Command: `XPLAT_APP_DIR=rx03b python3 tools/device-smoke/xplat_config.py --only X-03 --devices M,W`, result results/xplat-config-20261113T124936Z.json (rc 0).

- PASS X-03:x03_bytes:M>W 112s sha256 equal on laner-x03b-w and in the repository for 5 line-ending shapes
- PASS X-03:x03_bytes:W>M 33s sha256 equal on laner-x03b-m and in the repository for 5 line-ending shapes
- PASS X-03:x03_merge:M>W 137s three-way merge kept CRLF on every line
- PASS X-03:x03_merge:W>M 159s three-way merge kept CRLF on every line
- PASS X-03:x03_eol_edit:M>W 142s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written
- PASS X-03:x03_eol_edit:W>M 197s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written
(The cleanup of the run printed an IndexError from the harness's repository delete; the checks were already recorded.)
