# w8 evidence redo (winpool-13, 2026-11-10/11)

Device build: **dev-2d9866d86** (commit 2d9866d86db1ad972a22f7eba27d33b85415b407, verified via `pb version --json` on the runner).
Machine: w8-13-9d7677. Peer: hp (online). Runner healthy; no reruns needed (no FAIL, no SSH banner timeout).
Receipt /home/ubuntu/pb-wt/spark/handoff/winpool-13-install.json was gone, so rebuilt as results/winpool-13-install.json (minimal `pb install --json` envelope with the on-device exe path found via `ssh winpool-13`).

## (1) Command rows C236,C182,C186,C221 — PASS
File: tools/device-smoke/results/w8-rows-windows-13.out
Cmd: `env -u VICTUS_PASS SMOKE_WINDOWS_SSH=winpool-13 PB_SMOKE_WINDOWS_PB='<device pb.exe>' python3 command_rows.py w8-13-9d7677 --peer hp --only C236,C182,C186,C221`
{"check": "command-row", "row": "C221", "command": "pb update status", "status": "PASS", "failures": []}
{"check": "command-row", "row": "C186", "command": "pb config sync path", "status": "PASS", "failures": []}
{"check": "command-row", "row": "C236", "command": "pb config path", "status": "PASS", "failures": []}
{"check": "command-row", "row": "C182", "command": "pb config status", "status": "PASS", "failures": []}

## (2) terminal-entry — PASS
Result: tools/device-smoke/results/windows-20261110T232153Z.json
Output: tools/device-smoke/results/windows-output/20261110T232153Z-terminal-entry.out
{"at": "20261110T232153Z", "results": [{"id": "terminal-entry:victus", "status": "PASS", "seconds": 83, "detail": "{\"check\": \"terminal-entry\", \"target\": \"hp\", \"status\": \"PASS\", \"root_menu\": true, \"session_catalog\": true, \"root_attach\": true}"}]}

## (3) status-bar — PASS
Result: tools/device-smoke/results/windows-20261110T232417Z.json
Output: tools/device-smoke/results/windows-output/20261110T232417Z-status-bar.out
{"at": "20261110T232417Z", "results": [{"id": "status-bar:victus", "status": "PASS", "seconds": 126, "detail": "{\"check\": \"status-bar\", \"target\": \"hp\", \"status\": \"PASS\", \"preferences_persist\": true, \"preview\": true, \"reset\": true, \"live_reserved_row\": true}"}]}

## (4) network-drop — PASS
Result: tools/device-smoke/results/windows-20261110T232634Z.json
Output: tools/device-smoke/results/windows-output/20261110T232634Z-network-drop.out
{"at": "20261110T232634Z", "results": [{"id": "network-drop:victus", "status": "PASS", "seconds": 131, "detail": "{\"check\": \"network-drop\", \"target\": \"hp\", \"status\": \"PASS\", \"drop_seconds\": 20, \"same_shell\": true, \"input_exactly_once\": true}"}]}

## (5) F3-17 two,reconnect x2 from Unix client — PASS/PASS
File: tools/device-smoke/results/w8-f317-run-13.out (16 lines: 6 PASS + RESULT PASS + RUNx_EXIT=0, twice)
Method: fresh w8-13-f317a container on pbvm2 (~/laneh/mkct.sh + enroll.py, same personal account as w8-13-9d7677); linux pb dev-88512053 built from clean `git archive origin/main` (88512053247c8d249931f21dffca27a4cc661239, explicit GOOS=linux GOARCH=amd64); scripts staged from the archive; `python3 terminal_matrix.py w8-13-9d7677 --only two,reconnect` twice.
Both runs: N1 two-windows 5/5 PASS, N15 reconnect PASS, RESULT PASS (RUN1_EXIT=0, RUN2_EXIT=0).
Cleanup: revoked machine_bfb39fce-de14-4500-8a6e-427cc7705302 by exact id (confirmed, state disconnected), removed container w8-13-f317a and /tmp files on pbvm2.
