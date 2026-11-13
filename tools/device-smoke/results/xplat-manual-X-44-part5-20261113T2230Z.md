# X-44 part 5 by hand: Mac <-> Windows, both with two OS users, on main ac409910b (UTC 2026-11-13 ~21:50-22:35, IST 2026-11-14 ~03:20-04:05)

Build on every machine: dev-ac409910b (`pb version` of each: x44n-m-b, x44n-w-b, macc2, zoen2 all print "Version dev-ac409910b").
Throwaway accounts B and C (fresh console signups), prod server. Own machines: Mac macpool-2 (macOS, arm64) and Windows winpool-4,
nothing from the shared fleet.
Mac host: OS user runner = account B (x44n-m-b), OS user macc2 (uid 502, passwordless sudo, own login keychain unlocked,
paired from a LaunchAgent bootstrapped into user/502 as in part 4) = account C (x44n-m-c).
Windows host: OS user runneradmin = account B (x44n-w-b), local administrator zoen2 (scheduled task logon) = account C (x44n-w-c).

## The part 4 failure: cold first contact between the two account-B machines (runneradmin <-> Mac runner): PASS
Both machines installed and paired seconds before, no earlier traffic between them, first `pb exec` each way:
```
W->M  rc 0  9.5 s  runner
M->W  rc 0  7.6 s  runnervmm7ud1\runneradmin
```
(part 4 on dev-d77a40713: 3 of 3 first W->M tries and 2 of 3 first M->W tries failed with "could not reach the target machine".)

## Every user of each OS as client: PASS (own account reached, the other account's machines refused)
```
Mac B (runner)       -> x44n-w-b   7.1 s  runnervmm7ud1\runneradmin
Mac B (runner)       -> x44n-w-c   3.7 s  pb: No machine has that name or ID.
Mac B (runner)       -> x44n-m-c   3.5 s  pb: No machine has that name or ID.  (the other user of its own host)
Mac C (macc2)        -> x44n-w-c         runnervmm7ud1\zoen2
Mac C (macc2)        -> x44n-w-b         pb: No machine has that name or ID.
Mac C (macc2)        -> x44n-m-b         pb: No machine has that name or ID.
Win B (runneradmin)  -> x44n-m-b   7.3 s  runner
Win B (runneradmin)  -> x44n-m-c   4.3 s  pb: No machine has that name or ID.
Win C (zoen2)        -> x44n-m-c  19.3 s  macc2
Win C (zoen2)        -> x44n-m-b  15.8 s  pb: No machine has that name or ID.
```
Each user's `pb machine list` shows only its own account's two machines; assigned_ip per account (the gateway routes by the caller's OS user):
runner/runneradmin: x44n-m-b 127.100.0.2, x44n-w-b 127.100.0.3; macc2/zoen2: x44n-m-c 127.100.0.2, x44n-w-c 127.100.0.3.

## Simultaneous, all four users at once (6 execs each): PASS
```
Mac B   -> x44n-w-b   6x runnervmm7ud1\runneradmin
Win B   -> x44n-m-b   6x runner
Win C   -> x44n-m-c   6x macc2
Mac C   -> x44n-w-c   6x runnervmm7ud1\zoen2
```

## Refusal page for an OS user without a daemon: PASS (HTTP/1.1 403 "This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.")
Mac gateway http://127.100.0.1/: root and nobody. Windows gateway: a local administrator without pb (zoen3) and NT AUTHORITY\SYSTEM
(scheduled task). The Windows users with their own daemon get 421 (proxied, bare IP).

## Observation, not a failure of this row: a Windows PC where IIS already owns port 80
winpool-4 has IIS (W3SVC/WAS, HTTP.sys PID 4) running. With it running before pb, `pb doctor` said "Browser URLs are off because another program is
using port 80/443 (Windows HTTP service (HTTP.sys, for example IIS), PID 4). Recovery: Stop that program, and Paperboat turns browser URLs back on by itself."
(port and holder named), while a request to the gateway timed out silently (curl rc 28, no page). After `Stop-Service W3SVC, WAS` the gateway
listened on 127.100.0.1:80 within 45 s, but `pb doctor` still printed the same warning at 45 s (stale message). After starting IIS again the
gateway kept its listener and `pb doctor` printed daemon: pass. The refusal checks above ran with IIS stopped.
Harness note: `pb` run as macc2 through `ssh ... sudo -u macc2` (no launchd user session, keychain locked) fails with "could not finish the command"
(local_io); the Mac C checks ran from a LaunchAgent in user/502 with the keychain unlocked, as in part 4.
Main advanced after the build (ef42e84a9): changes only in filetransfer publication and Windows firewall-rule install, not in exec, reach or the gateway.
