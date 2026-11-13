# X-09 second pass (R-X-09B), 2026-11-13 21:00 - 2026-11-14 01:05 IST, by hand with a throwaway harness (scratch, not committed)

Builds: see each section (A: macpool-1 runner dev-79203d6e5 -> dev-5228278ac; C: fleet Mac VM and lanex2-l2 on dev-b6a4df91c). Fleet Mac VM lanex2-m and lanex2-l2 were upgraded by FLEET3 between runs (dev-5228278ac, then dev-b6a4df91c). victus (lanex2-w, the real PC): dev-cf0c0a620 for the first reboot run, dev-e4515714f (today's main code) for the logoff run.

## A. Mac in-place upgrade with the worker running (macpool-1, own account partner x09b-l (pbvm2 container))
- pb version L2 (partner): dev-5228278ac; M (macpool-1 runner, x09b-m): dev-79203d6e5
- apply -> healthy on both: PASS; M launchd:
```
8965	0	com.pinksaucepasta.paperboat.runtime-config
72717	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboat.runtime-config.plist
com.pinksaucepasta.paperboatd.plist
gui=501
```
- edits both ways before upgrade: PASS
- (manual) worker pid via launchctl print gui/501 before upgrade: 8965 (state running)
- worker (com.pinksaucepasta.paperboat.runtime-config) pid before upgrade: 8965 state=healthy
- install over M while worker ran: version=dev-5228278ac
- edit made on L2 during the upgrade arrived on M: PASS
- worker pid after upgrade: 8965 (before 8965); state=healthy; M version dev-5228278ac
```
8965	0	com.pinksaucepasta.paperboat.runtime-config
16331	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboat.runtime-config.plist
com.pinksaucepasta.paperboatd.plist
gui=501
```
- edits both ways after upgrade: PASS (M->L2 and L2->M)
- unassign M: state=disabled; agents now:
```
16331	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboatd.plist
gui=501
```
- L2 edit after unassign did not reach M: PASS
- cleanup done

## C. macOS loginwindow logout/login on the fleet Mac VM (lanex2-m), partner lanex2-l2 (anvit)
- pb version L2: dev-b6a4df91c; M (lanex2-m): dev-b6a4df91c
- apply -> healthy on both, edits both ways: PASS; M: admin            console      Nov 13 17:53  / agents: / 12019	0	com.pinksaucepasta.paperboat.runtime-config / 6141	0	com.pinksaucepasta.paperboatd
- after loginwindow logout (event aevtrlgo as uid 501): agents:
- state of M while logged out: healthy
- edit on L2 during logout reached M while logged out: no (expected: worker is a user agent)
- login did not return by killing loginwindow (agents:); rebooting for auto-login
- login via reboot auto-login: admin            console      Nov 13 18:23  / agents: / 477	0	com.pinksaucepasta.paperboat.runtime-config / 485	0	com.pinksaucepasta.paperboatd
- fixture tunnel to the VM reopened on port 48609 at 23:55:38
- after login: M healthy, edit made while logged out arrived: PASS
- edits both ways after login: PASS
- exec lanex2-m -> lanex2-l2 rc=0
- exec lanex2-l2 -> lanex2-m rc=0
- cleanup done; M: admin            console      Nov 13 18:23  / agents: / 485	0	com.pinksaucepasta.paperboatd

## B. Windows (victus, real PC)
### B1 reboot run, 22:48 IST, dev-cf0c0a620 (partner x09b-m on a macOS runner)
- apply -> healthy on W and M, RDP session of Pujan opened and logged off (`logoff 3`), services Running after it, edits M->W and W->M arrived after the logoff: PASS (same run shape as B4 below)
- reboot: `shutdown /r /t 5 /f` at 22:48:24; all Paperboat services Running 71 s after the command, `pb version` answers: PASS
- edit made on M while victus was down did NOT arrive within 300 s: the fixture (ssh -R from oracle carrying the repository to victus) ended with the reboot; the harness did not reopen it. Not a product result; rerun with the tunnel reopened was never reached (see gaps).
### B6 logoff run, 00:29 IST, dev-e4515714f, partner x09b-m (macpool-5, dev-e4515714f)

## B. Windows (victus, real PC) logoff and reboot with the worker running; partner x09b-m (macpool-1)
- pb version W (lanex2-w victus): dev-e4515714f; M (x09b-m, macOS runner): dev-5228278ac
- apply -> healthy on W and M: PASS; services: `PaperboatCarrier-uba6bceb9290ebd807a30e102-14a078f7b5f40efc198f4b9b697f102a=Running | PaperboatHostd-uba6bceb9290ebd807a30e102-14a078f7b5f40efc198f4b9b697f102a=Running | PaperboatLocalDaemon-uba6bceb9290ebd807a30e102=Running | PaperboatRuntimeConfig-uba6bceb9290ebd807a30e102=Run
- edits both ways (M<->W) after apply: PASS
- quser before: `No User exists for *`
- quser with my RDP session: `USERNAME              SESSIONNAME        ID  STATE   IDLE TIME  LOGON TIME /  pujan                 rdp-tcp#0           5  Active          .  11/14/2026 12:29 AM` -> my session id(s) ['5']
- edit M->W while the interactive session was active: PASS; services `PaperboatCarrier-uba6bceb9290ebd807a30e102-14a078f7b5f40efc198f4b9b697f102a=Running | PaperboatHostd-uba6bceb9290ebd807a30e102-14a078f7b5f40efc198f4b9b697f102a=Running | PaperboatLocalDaemon-uba6bceb9290ebd807a30e102=Running | PaperboatRuntimeConfig-uba6bce
- logoff 5: ok
- quser after logoff: `No User exists for *`; services `PaperboatCarrier-uba6bceb9290ebd807a30e102-14a078f7b5f40efc198f4b9b697f102a=Running | PaperboatHostd-uba6bceb9290ebd807a30e102-14a078f7b5f40efc198f4b9b697f102a=Running | PaperboatLocalDaemon-uba6bceb9290ebd807a30e102=Running | PaperboatRuntimeConfig-uba6bceb9290ebd807a30
- after logoff: state healthy, edits M->W and W->M arrived: PASS
- reboot NOT run: outside the victus power window (00:34 IST)
- cleanup done

### Windows apply that stayed `pending` (product question, failures.md)
- 4 applies on victus (dev-5228278ac 23:30; dev-b6a4df91c 00:04 region; dev-e4515714f 00:44 and 00:55) stayed server-side state `pending` (remote_revision, manifest_health empty) for 240 s although PaperboatRuntimeConfig was Running and the peer was healthy; other applies on the same builds were healthy in under a minute. Victus then had three PaperboatSessionOwner generations (14a078f7 Running, c42e7e5a Running, 902b082c Stopped). Not diagnosed: no worker log found by the harness.

## Not run / gaps
- W: reboot with an edit made during it arriving after it (B1 saw the reboot and services, not the edit); in-place upgrade on victus itself (cycle-2 evidence is on a hosted Windows runner); applies that stay pending (above).
- L: not rerun on today's main (container x09b-l on pbvm2 was removed when the victus slot arrived; the cycle-2 L evidence stands).
