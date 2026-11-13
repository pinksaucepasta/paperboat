# X-44 part 4 by hand: Mac host with two OS users, UTC 2026-11-13 ~14:00-14:45 (IST ~19:30-20:15)

Build on every machine: dev-d77a40713 (main d77a40713). Throwaway accounts B and C (fresh console signups), prod server.
Machines: Linux container rx44-l on pbvm2 with OS users pbt (account B, x44r-l) and pbt2 (account C, x44r-lc, passwordless sudo);
Windows runner winpool-6 with runneradmin (B, x44r-w) and zoec (C, x44r-wc, local administrator via scheduled task);
Mac macpool-5 (macOS 26.7.2 arm64) with OS users runner (B, x44r-m-2) and macc (uid 510, C, x44r-mc-2). (x44r-m, x44r-mc, x44r-mc1
are records of an earlier macpool-5 runner that GitHub replaced mid-run: offline, ignored.)

## How the second Mac user was paired (the earlier SIGHUP was the ssh session, not pb)
A launched-over-ssh `pb pair` has no session. What the second user needs, all provided by hand here:
1. passwordless sudo for macc (pb bootstraps its LaunchDaemons with `sudo -n`);
2. an unlocked login keychain (macOS always stores the credential there; without it pb says "Paperboat could not use this account's
   credential store. On macOS over SSH the login keychain is locked: run `security unlock-keychain` first ..." and exits rc 1, clear and no hang);
3. a launchd user domain: pair run from a LaunchAgent bootstrapped into user/510 (`sudo launchctl bootstrap user/510 ...`,
   LimitLoadToSessionType Background). The same pair run from a system-domain LaunchDaemon (UserName macc) fails with the keychain error
   even after `security unlock-keychain` succeeded; run in user/510 it paired, doctor healthy (daemon ready, hostd answers).
A real second user gets 2 and 3 from a GUI login (fast user switching); the runner has no such session.

## Linux clients of the Mac host (B=pbt, C=pbt2): PASS
== Linux clients -> Mac host (B=pbt, C=pbt2)
pbt exec x44r-m-2:
runner
pbt exec x44r-mc-2:
pb: No machine has that name or ID. Run `pb machine list` to see your machines.
pbt2 exec x44r-mc-2:
macc
pbt2 exec x44r-m-2:
pb: No machine has that name or ID. Run `pb machine list` to see your machines.

## Windows clients of the Mac host (B=runneradmin, C=zoec)
```
runneradmin exec x44r-m-2:
pb: Paperboat could not reach the target machine. It may be offline, or its network or Paperboat ser
runneradmin exec x44r-mc-2:
pb: No machine has that name or ID. Run `pb machine list` to see your machines.
zoec exec x44r-mc-2:
macc

zoec exec x44r-m-2:
pb: No machine has that name or ID. Run `pb machine list` to see your machines.

```
Windows B (runneradmin) -> Mac runner: first tries failed 'could not reach' (3 times over ~25 min incl. the loop), the next two returned 'runner'; Mac runner -> Windows B x44r-w: 'could not reach' 2 of 3 first tries, then 'runnervmm7ud1\runneradmin'. Windows C (zoec) <-> macc passed on the first try both ways. Same first-contact failure as the earlier part (cold relay path). NOT a clean pass: M<->W stays [~].

## Simultaneous (Linux B and C loops 6x each at once, Mac runner loop at once, macc loop 6x)
```
== Linux clients -> Mac host, 6x each, simultaneous
pbt(B)->x44r-m-2       6 runner
pbt2(C)->x44r-mc-2       6 macc
== Mac runner (B) loops 6x
      6 pbt
== simultaneous macc loops (6x)
   6 pbt2
```

## Mac gateway refusal page for OS users without a daemon (root, nobody): PASS
```
-- root
HTTP/1.1 403 Forbidden
This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.
-- nobody
HTTP/1.1 403 Forbidden
This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.
```

## The Mac host own users resolve only their account machines
```
macc version: Version dev-d77a40713
macc(C) exec x44r-lc:
pbt2
macc(C) exec x44r-wc:
runnervmm7ud1\zoec
macc(C) exec x44r-l:
pb: No machine has that name or ID. Run `pb machine list` to see your machines.
macc(C) exec x44r-m-2:
pb: No machine has that name or ID. Run `pb machine list` to see your machines.
macc(C) machine list names:
x44r-lc online
x44r-mc offline
x44r-mc1 offline
x44r-mc-2 online
x44r-wc online
mac runner (B): exec x44r-l -> pbt, exec x44r-lc -> No machine has that name or ID, loop 6x pbt
```

## pb version: Linux pbt/pbt2, Mac runner/macc, Windows runneradmin all Version dev-d77a40713.
