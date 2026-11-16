# F25-10 M and W: session recordings in the personal workspace on macOS and Windows, 2026-11-16 (IST ~16:30-18:00)

Build `dev-763a4e11d` (main with f24ee62dd, 13d3fec3d), server .29, console on main. One scripted run per device and account (`devrun.py`, scratch harness: installs the build, `pb login` with browser approval, `pb env unlock`, `pb connect` + `pb recordings start`, list, export, play, a web-terminal "Start recording" in the console, browser replay of both recordings, then revokes the computer's record). Markers are typed into the session and must come back in the export, the CLI replay and the browser screen.
- macOS 26.6.2: the pbmac2 Tart VM (arm64, user admin, shared Mac lock held for each run, 5 min cooldown). Windows: GitHub Windows runner winpool-3 (Windows 11 10.0.26100, runneradmin), pb as a service install.
- Free: a fresh account (mail.tm sign-up, Billing shows "Plan Free"), vault created in the console, recording key from the console setup card. Pro: the operator account (Billing "Pro $8/month").

## macOS, Free account
```
PASS login prints an approval link
PASS signed in  (recfreec…)
PASS this computer is a machine of the account  (managedlmachine)
PASS pb env unlock  (ENV vault operation completed.)
PASS pb recordings start while attached  (rec_9f2d7692)
PASS terminal session ran
PASS pb recordings list shows it ended
PASS pb recordings export (asciicast has the markers)  (2 FREE-MAC-BEFORE 2 FREE-MAC-ONE 1 FREE-MAC-T 1 FREE-MAC-TWO)
PASS pb recordings play replays it
PASS web terminal: Start recording shows REC  (phase: attached /terminals/terminal_86820516-2c01-458b-88f0-629e5f1a55)
PASS the web terminal recording is listed and ended  (rec_dc37056a)
PASS browser replay of the web terminal recording  (0:05 / 0:05 error: null)
PASS browser replay  (0:12 / 0:12 error: null)
RESULT ALL PASS (13/13) rec rec_9f2d7692-f2ed-4084-92fc-3a752094e23f
```

## macOS, Pro account
```
PASS login prints an approval link
PASS signed in  (pprbtdev…)
PASS this computer is a machine of the account  (managedlmachine)
PASS pb env unlock  (ENV vault operation completed.)
PASS pb recordings start while attached  (rec_0101c64b)
PASS terminal session ran
PASS pb recordings list shows it ended
PASS pb recordings export (asciicast has the markers)  (1 PRO-MAC- 2 PRO-MAC-BEFORE 1 PRO-MAC-ONE 2 PRO-MAC-TWO)
PASS pb recordings play replays it
PASS web terminal: Start recording shows REC  (phase: attached /terminals/terminal_79453d8d-1621-4b4f-8a73-5973fadc36)
PASS the web terminal recording is listed and ended  (rec_7c3eccde)
PASS browser replay of the web terminal recording  (0:05 / 0:05 error: null)
PASS browser replay  (0:12 / 0:12 error: null)
RESULT ALL PASS (13/13) rec rec_0101c64b-ede0-4a8c-8534-b6fdfc5cd373
```

## Windows, Free account
```
PASS login prints an approval link
PASS signed in  (recfreec…)
PASS this computer is a machine of the account  (runnervmm7ud1)
PASS pb env unlock  (ENV vault operation completed.)
PASS pb recordings start while attached  (rec_a7896ea6)
PASS terminal session ran
PASS pb recordings list shows it ended
PASS pb recordings export (asciicast has the markers)  (2 FREE-WIN-BEFORE 2 FREE-WIN-ONE 2 FREE-WIN-TWO)
PASS pb recordings play replays it
PASS web terminal: Start recording shows REC  (phase: attached /terminals/terminal_c8bb0a23-1501-4321-81f2-d203ebf100)
PASS the web terminal recording is listed and ended  (rec_62610be0)
PASS browser replay of the web terminal recording  (0:05 / 0:05 error: null)
PASS browser replay  (0:12 / 0:12 error: null)
RESULT ALL PASS (13/13) rec rec_a7896ea6-144d-4f6d-903b-ab20ec5b2f0d
```

## Windows, Pro account
```
PASS login prints an approval link
PASS signed in  (pprbtdev…)
PASS this computer is a machine of the account  (runnervmm7ud1)
PASS pb env unlock  (ENV vault operation completed.)
PASS pb recordings start while attached  (rec_088a84b6)
PASS terminal session ran
PASS pb recordings list shows it ended
PASS pb recordings export (asciicast has the markers)  (2 PRO-WIN-BEFORE 2 PRO-WIN-ONE 2 PRO-WIN-TWO)
PASS pb recordings play replays it
PASS web terminal: Start recording shows REC  (phase: attached /terminals/terminal_0d652af0-bbbc-427b-b340-a90cde594b)
PASS the web terminal recording is listed and ended  (rec_b3888d3d)
PASS browser replay of the web terminal recording  (0:05 / 0:05 error: null)
PASS browser replay  (0:12 / 0:12 error: null)
RESULT ALL PASS (13/13) rec rec_088a84b6-991d-4264-8880-d354b86ad448
```

Earlier runs of the same flow without the web-terminal step (Mac Free rec_c6228d90..., Mac Pro rec_e1efa4de-eab9-494f-be23-35f2ac526eb9, Windows Free rec_9b623f89-c543-434c-8a8f-b0f50c55453c, Windows Pro rec_d281cbcd-d6fc-4357-8160-95d34f8e50af) passed their CLI and browser steps too (one first Mac Free run failed only the 'list shows it ended' step: the index lags a few seconds; the harness now polls).
Harness failures fixed on the way (not product): a shared approval-link file between two parallel runs, a PowerShell stdin that ssh never closes (password file instead), CLIXML noise, an uninstall that keeps removing files after it returns. Not covered: Linux Teams bucket and Linux runs are in recordings-personal-L-20261116.md; Teams upload on Mac/Windows hosts was not run (the upload loop is the same host code; bucket PASS on Linux).
