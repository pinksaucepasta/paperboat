# X-27 last gaps, by hand (2026-11-14 00:00-00:50 IST), lane R-X-27D

Own machines in the xplat2 test account: L = `rx27d-l` (Linux container on pbvm2), M = `rx27d-m`/`rx27d-mo` (macpool-3, macOS), W = `rx27d-w`/`rx27d-wo` (winpool-3, Windows Server). `pb version` read on all three: dev-2830fb517 (main 2830fb517). Main has since moved only in file transfer, the Windows VT output and the tunnel local peer, not in clock paths. Six exec directions L/M/W ok after the install.

## 1. Terminal replay on a Windows target across a DST crossing (cmd.exe session, numbered loop, client killed, replay later)
Custom Windows zone `RxFallN` (UTC+1 DST ending at a chosen UTC instant, `tzutil /s`, pb services restarted). Three sessions per crossing: two clients on L, one on M.
| Crossing | Local clock | Result (3 of 3 sessions) |
|---|---|---|
| 17:52:00Z, loop 240 lines | 18:50:51+01:00 -> 17:55:24+00:00 | lines 19..240 in order, no gap inside; lines 1..18 are older than the 32 KiB replay window (`attachmentReplayLimit`, manager_attach.go) |
| control, same loop, no crossing (18:00-18:05Z) | | identical: 19..240, in order (so the window, not the clock) |
| 18:22:00Z, loop 150 lines | 19:21:47+01:00 -> 18:25:24+00:00 | 1..150, no gap, in order, 3 of 3 |
| 18:40:00Z, loop 150 lines | 19:39:51+01:00 -> 18:43:24+00:00 | 1..150, no gap, in order, 3 of 3 |
The one lost line (54 of 80) in R-X-27B did not repeat in 9 replays over 3 crossings.

## 2. Recordings and audit rows shown in the viewer's zone (team rx23-team: paid sandbox with recording key; my three machines transferred to it, owner granted terminal and exec)
One recording on each OS: L rec_5b8ffda2 18:09Z, M rec_b81532bf 18:10Z, W rec_088d0d30 18:46Z (all `ended`, 14 s). Audit rows machine.access_started / recording_started / recording_stopped for each.
- Console /recordings and /activity in headless Chromium with timezoneId UTC, Asia/Tokyo, America/Los_Angeles: every row's text equals the same instant in that zone ("6:10 PM" / "3:10 AM GMT+9" / "10:10 AM PST"; title attribute with the zone).
- CLI `pb recordings list` in the local zone on the real machines: L 18:09 UTC, M (zone Los Angeles) 10:10 PST, W (Tokyo) 03:46 JST, and on the controller with TZ=Asia/Tokyo 03:10 JST, TZ=America/Los_Angeles 10:10 PST.
- `pb team activity` prints UTC RFC 3339 on every machine (documented: server/docs/teams-business.md).

## 3. Scheduled update fires at its local time (official 2026.11.13.0 on each; dev builds report automatic updates off)
`pb update settings --time <now+5 min>`; the updater's `maintenance.json` is written at the fire.
| OS | Zone | Set | maintenance.json written |
|---|---|---|---|
| Linux (container rx27d-lo) | Asia/Tokyo | 03:21 | 03:21:01 JST, local_date 2026-11-14, pending false |
| macOS (rx27d-mo) | America/Los_Angeles | 10:53 | 10:53:01 PST, local_date 2026-11-13, pending false |
| Windows (rx27d-wo) | Tokyo Standard Time | 03:57 | 03:57:01 JST, local_date 2026-11-14, pending false |
No newer release existed, so nothing installed; the fire is the check.

## Not run / notes
- Minutes-long skew passes were stopped on the coordinator's order (the row promises seconds; a Windows clock 3 s behind worked on fb752c7b8). A pass with Mac +3 min was valid (doctor named it, six directions ok). The Windows runner's Hyper-V time sync (`vmictimesync`) puts a set-back clock right within 5 s, so a Windows set-back needs `Stop-Service vmictimesync`; I restored it (Automatic, Running) and both clocks and NTP.
- `pb doctor` on a Windows runner whose clock was set back is not a valid check unless vmictimesync is stopped.
- Fixed: the `clock_skew` API message now matches `pb doctor` (19179e79e, TestClockSkewMessageNamesTheSetBackDelay, CI run 43471150653).
