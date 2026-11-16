# F25-10 L: session recordings in the personal workspace (Free and Pro), 2026-11-16 (IST 13:00-13:20)

Build `dev-4188ece01` (contains f24ee62dd + e7a0aa89a), server .29, console on main. Linux container on hp (Ubuntu 24.04 systemd, `pb install`ed, added with `pb login`), self-connect (`pb connect` to its own machine).
Evidence kept: terminal output below; screenshots were taken by the headless scripts and are not committed.

## Free account (plan Free confirmed on its Billing page: "Plan Free", 5 GB, 100 machines)
Fresh account (mail.tm sign-up), ENV vault created in the console, machine `0648f39ed872` = `machine_5c24041a-1091-41a8-8cf3-52d59a9af196`.
| Step | Result |
|---|---|
| console Terminals > Recordings in the personal workspace, no key yet | setup card "Record your terminals"; "Turn on recording" made the key, no plan prompt (PASS) |
| `pb recordings list` before any recording | `No recordings.` (PASS) |
| `pb env unlock`, `pb connect 0648f39ed872`, `pb recordings start --json` while attached | `rec_545bfc85-f316-466a-bed8-072d5df3f07d`, trigger manual, started_by = the account (PASS) |
| session ended, `pb recordings list` | `rec_545bfc85-...  0648f39ed872  2026-11-16 07:38 UTC  38s  manual  ended` (PASS) |
| `pb recordings export <id> -o file` | asciicast v2, 1062 bytes, markers FREE-BEFORE, FREE-MARK-ONE, FREE-MARK-TWO each present (PASS) |
| `pb recordings play --speed 20 <id>` | the three markers replayed (PASS) |
| browser replay `/recordings/<id>` (vault unlocked in the page) | phase ready, screen shows the three commands and `exit`, clock `0:38 / 0:38` (PASS) |
| web terminal: `Start recording` button in the personal workspace, type `echo WEB-MARK-ONE`, `Stop recording` | REC shown, `rec_837627da-9fd6-4634-8c99-d269fb1daf7c` indexed (7 s, ended), browser replay shows `echo WEB-MARK-ONE` and its output, `0:05 / 0:05` (PASS) |

## Pro account (the operator account, Billing: "Pro $8/month")
Same flow on `a1ac14b797da`: key from the console, `pb connect` + `pb recordings start`, `rec_971cda74-df08-4309-9e9d-b962ef52ae91` (38 s), `pb recordings list/export/play` show BEFORE-REC, RECMARK-ONE, RECMARK-TWO, browser replay `0:39 / 0:39` with the same text (PASS).

## Unit and integration tests
`TestPostgresPersonalRecordings`, `TestPostgresRecordingsEndToEnd`, `TestPostgresRecordingsOnASharedPersonalMachine`, `TestRecordingReplayAdmissionReachesBothEndpointsOnPostgres`, `TestPostgresRecordingsArchive` (CI run for f24ee62dd), `TestPersonalScope`, console `bun run check`.

## Not covered here
Mac and Windows (M and W cells stay open). Team recordings and the bucket upload were not re-run on devices (unchanged code paths; their PostgreSQL tests pass).
Observed, not part of this row: right after `pb login` the first `pb recordings export` answered "credential secret not found" until `pb env unlock` was run again (the host service restarted and dropped the daemon's vault custody).
