# X-09 config worker, L and W on dev-08d3c4462 (lane R-X-09C), 2026-11-14 05:50-06:40 IST

Fleet: lanex2-l2 (Linux container on anvit, systemd user manager, linger), lanex2-w (victus, real Windows PC), partner each way; all three on dev-08d3c4462 (main 08d3c4462, contains 42172c232 and f44ad1a30). Repository fixture on anvit. Scripts were scratch (not committed): apply, edits via the harness (xplat_config), `loginctl`, `docker restart`, `shutdown /r`.

## Findings on the way (fixed or explained)
- Runs 1-2 (dev-6fe9473fb, 03:10 and 03:38 IST): L->W edit arrived, W->L never did. Cause: an edit written by the (elevated) ssh session on victus is owned by BUILTIN\Administrators and the Windows worker only accepted files owned by the user, so it skipped them as `unsafe_permissions`. A user editing from an "Administrator" terminal hits the same. Fixed in f44ad1a30: owner may be the user, Administrators or SYSTEM and nobody else may have write access; other owners/writers are skipped with their own reason (`owned_by_other_user`, `writable_by_other_users`) that `pb config status` prints per file. Test TestUnsafeSnapshotReasonOwnersAndWriters PASS on victus; actions run 43521735256. On dev-08d3c4462 the same elevated-ssh writes now sync both ways without any harness workaround.
- `loginctl terminate-user tester` is not a logout: it stops user@1001.service, which logind does not restart even with linger, so paperboatd and the worker stay down (the fleet machine lanex2-l2 was offline for ~25 min until I ran `systemctl start user@1001` as root; restored, exec 6/6). The logout equivalent used below is a real login session (`su -l tester`, pam_systemd) ended with `loginctl terminate-session`.

## L (lanex2-l2, partner lanex2-w), results of the 05:50 IST run
- apply -> healthy on L and W: PASS
```
MainPID=53716 PrivateTmp=no ActiveState=active SubState=running 
0
enabled
```
- DIAG lanex2-w config status:  managed, 0 pending clean / xd27-l: disabled, , manifest , 0 managed, 0 pending clean / xd27-m: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-2: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-3: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-4: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-5: disabled, , manifest , 0 managed, 0 pending clean / xd27-w: disabled, , manifest , 0 managed, 0 pending clean / xd27-w-2: disabled, , manifest , 0 managed, 0 pending clean / 
- DIAG lanex2-w sees file: True
- DIAG lanex2-l2 config status:  managed, 0 pending clean / xd27-l: disabled, , manifest , 0 managed, 0 pending clean / xd27-m: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-2: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-3: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-4: disabled, , manifest , 0 managed, 0 pending clean / xd27-m-5: disabled, , manifest , 0 managed, 0 pending clean / xd27-w: disabled, , manifest , 0 managed, 0 pending clean / xd27-w-2: disabled, , manifest , 0 managed, 0 pending clean / 
- DIAG lanex2-l2 sees file: False
- DIAG lanex2-l2 journal: Nov 13 19:10:08 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 19:20:38 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / Nov 13 19:25:22 76e658e8b65b systemd[128]: Stopping paperboat-runtime-config.service - Paperboat config sync... / Nov 13 19:25:22 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 19:25:22 76e658e8b65b systemd[128]: paperboat-runtime-config.service: Consumed 1.232s CPU time, 23.5M memory peak, 0B memory swap peak. / Nov 13 19:32:07 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / Nov 13 19:43:18 76e658e8b65b systemd[128]: Stopping paperboat-runtime-config.service - Paperboat config sync... / Nov 13 19:43:18 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 19:43:18 76e658e8b65b systemd[128]: paperboat-runtime-config.service: Consumed 2.581s CPU time, 26.4M memory peak, 0B memory swap peak. / Nov 13 19:58:17 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / Nov 13 20:09:26 76e658e8b65b systemd[128]: Stopping paperboat-runtime-config.service - Paperboat config sync... / Nov 13 20:09:26 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 20:09:26 76e658e8b65b systemd[128]: paperboat-runtime-config.service: Consumed 2.496s CPU time, 26.2M memory peak, 0B memory swap peak. / Nov 13 20:47:36 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / Nov 13 20:58:47 76e658e8b65b systemd[128]: Stopping paperboat-runtime-config.service - Paperboat config sync... / Nov 13 20:58:47 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 20:58:47 76e658e8b65b systemd[128]: paperboat-runtime-config.service: Consumed 2.606s CPU time, 24.9M memory peak, 0B memory swap peak. / Nov 13 21:40:36 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / Nov 13 21:46:14 76e658e8b65b systemd[128]: Stopping paperboat-runtime-config.service - Paperboat config sync... / Nov 13 21:46:14 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 21:56:54 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / Nov 13 22:08:05 76e658e8b65b systemd[128]: Stopping paperboat-runtime-config.service - Paperboat config sync... / Nov 13 22:08:05 76e658e8b65b systemd[128]: Stopped paperboat-runtime-config.service - Paperboat config sync. / Nov 13 22:08:05 76e658e8b65b systemd[128]: paperboat-runtime-config.service: Consumed 2.300s CPU time, 23.7M memory peak, 0B memory swap peak. / Nov 13 22:08:34 76e658e8b65b systemd[128]: Started paperboat-runtime-config.service - Paperboat config sync. / 
- cleanup done


- pb version L dev-08d3c4462, W dev-08d3c4462
- apply -> healthy on L and W: PASS
```
MainPID=67119 PrivateTmp=no ActiveState=active SubState=running 
0
enabled
```
- edits both ways (L<->W) after apply: PASS
- logout equivalent: `loginctl terminate-user tester` (as root in the container) at 05:05:16
  rc=0 
- cleanup done


- pb version L dev-08d3c4462, W dev-08d3c4462
- apply -> healthy on L and W: PASS
```
MainPID=72740 PrivateTmp=no ActiveState=active SubState=running 
0
enabled
```
- edits both ways (L<->W) after apply: PASS
- logout equivalent: login session open: `c2 1001 tester - - active no -`; `loginctl terminate-session` at 05:54:11
  sessions after: `c2 1001 tester - - closing no -`; user@1001: active
- edit made on W during the user-session end arrived on L: PASS; state=healthy
  before: MainPID=72740 PrivateTmp=no ActiveState=active SubState=running  | 0 | enabled
  after: MainPID=72740 PrivateTmp=no ActiveState=active SubState=running  | 0 | enabled
- edits both ways after session end: PASS
- reboot equivalent: `docker restart lanex2-l2` at 05:58:08
- edit made on W during the container restart arrived on L: PASS; state=healthy
  before: MainPID=72740 PrivateTmp=no ActiveState=active SubState=running  | 0 | enabled
  after: MainPID=197 PrivateTmp=no ActiveState=active SubState=running  | 0 | enabled
- edits both ways after reboot: PASS
- exec lanex2-l2 -> lanex2-w rc=0
- exec lanex2-w -> lanex2-l2 rc=0
- unassign L: state=disabled
```
MainPID=0 PrivateTmp=no ActiveState=inactive SubState=dead 
not-found
```
- W edit after unassign did not reach L: PASS
- cleanup done

## W (lanex2-w victus, partner lanex2-l2), 06:03-06:37 IST run
- pb version W (lanex2-w): dev-08d3c4462; L (lanex2-l2): dev-08d3c4462

## W. Windows config worker on victus (real PC); partner lanex2-l2 (Linux container on anvit)
- pb version W (lanex2-w): dev-08d3c4462; L (lanex2-l2): dev-08d3c4462
- runtime\config-sync before apply: 5b53c171aca1987da1807df569551f1e 11/14/2026 06:03:34 | repository-credentials 11/14/2026 05:50:55 | repository-read 11/14/2026 05:51:17
- apply -> healthy on W and L: PASS; services: `PaperboatCarrier-uba6bceb9290ebd807a30e102-65948ea8b649fcb3f4732fb7499fb2bb=Running | PaperboatHostd-uba6bceb9290ebd807a30e102-65948ea8b649fcb3f4732fb7499fb2bb=Running | PaperboatLocalDaemon-uba6bceb9290ebd807a30e102=Running | PaperboatRuntimeConfig-uba6bceb9290ebd807a30e102=Run
- runtime\config-sync after apply: 7726cf61d66d27526b45716d5b850f16 11/14/2026 06:04:36 | repository-credentials 11/14/2026 06:04:10 | repository-read 11/14/2026 06:04:30
- edits both ways (L<->W) after apply: PASS
- ready to reboot at 06:08:42; waiting for the go file
- reboot: `shutdown /r /t 5 /f` at 06:30:00
- victus back, services Running 78s after the command: `PaperboatCarrier-uba6bceb9290ebd807a30e102-65948ea8b649fcb3f4732fb7499fb2bb=Running | PaperboatHostd-uba6bceb9290ebd807a30e102-65948ea8b649fcb3f4732fb7499fb2bb=Running | PaperboatLocalDaemon-uba6bceb9290ebd807a30e102=Running | PaperboatRuntimeConfig-uba6bceb9290ebd807a30
- pb version W after reboot: dev-08d3c4462
- fixture tunnel to victus reopened on port 37727 at 06:31:22 (it ended with the reboot)
- edit made on L while W was down arrived on W after the reboot; state=healthy: PASS
- after reboot edits both ways: PASS
- exec lanex2-l2 -> lanex2-w rc=0
- exec lanex2-w -> lanex2-l2 rc=0
- unassign W: state=disabled; services `PaperboatCarrier-uba6bceb9290ebd807a30e102-65948ea8b649fcb3f4732fb7499fb2bb=Running | PaperboatHostd-uba6bceb9290ebd807a30e102-65948ea8b649fcb3f4732fb7499fb2bb=Running | PaperboatLocalDaemon-uba6bceb9290ebd807a30e102=Running | PaperboatSessionOwner-uba6bceb9290ebd807a30e102-65948ea8b649
- L edit after unassign did not reach W: PASS
- cleanup done

## After the reboot: pbvm2 and pbvm4 Running, pbvm3 Saved (as before; AutomaticStartAction Start on pbvm2/pbvm4, Nothing on pbvm3); fleet pb version L/M/W dev-08d3c4462.
