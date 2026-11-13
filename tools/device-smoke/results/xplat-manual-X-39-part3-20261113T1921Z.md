# X-39 part 3: Mac VM (lanex2-m) and real Windows PC (lanex2-w, victus) service states, 2026-11-13 18:08-00:50 IST

Builds: lanex2-m dev-79203d6e5 (pbmac Tart VM, macOS 26.6.2, FileVault off, autologin admin), lanex2-w dev-cf0c0a620 (victus, Windows 11, services before sign-in, fast startup on, nobody signed in at the console). Observers: the macOS checks were observed from L and W (before 19:00 IST); the Windows checks from M only, because the Linux fleet machine runs on hp, which went down (load 250) and stayed down. victus was never slept, hibernated or shut down in this part (user rule); its restart is part 3b below, done before that rule.

## macOS (lanex2-m), observed from L and W
```
== baseline 12:38:40Z
lanex2-l -> lanex2-m: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Manageds-Virtual-Machine.local
lanex2-w -> lanex2-m: runtime=ok(2s) | transport=ok(4s) | ssh=ok(3s) | exec=ok Manageds-Virtual-Machine.local
== lock screen
locked
ModuleNotFoundError: No module named 'Quartz'
0
+-o Root  <class IORegistryEntry, id 0x100000100, retain 8>
-- screen locked (CGSessionScreenIsLocked=1 read from the session dictionary):
lanex2-l -> lanex2-m: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Manageds-Virtual-Machine.local
lanex2-w -> lanex2-m: runtime=ok(2s) | transport=ok(3s) | ssh=ok(3s) | exec=ok Manageds-Virtual-Machine.local
1 1 admin
Paperboat daemon service status: running
laneX2-l
-- fast user switching (SACSwitchToLoginWindow: admin session off the console, kCGSSessionOnConsoleKey=0, admin's launchd domain and paperboatd still running):
0
notlocked 0 admin
admin            console      Nov 12 14:59 
1
admin            /System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow
root             /Library/PrivilegedHelperTools/io.paperboat.machineguard
root             /System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow
lanex2-l -> lanex2-m: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Manageds-Virtual-Machine.local
lanex2-w -> lanex2-m: runtime=ok(2s) | transport=ok(3s) | ssh=ok(3s) | exec=ok Manageds-Virtual-Machine.local
Paperboat daemon service status: running
-- reboot with autologin (boot 12 s after the command):
reboot at 12:41:13Z
t+   6s runtime=ok transport=no
t+  19s runtime=ok transport=ok exec=ok
t+ 321s runtime=ok transport=no
t+ 327s runtime=ok transport=ok exec=ok
ready_at=19s
{ sec = 1794573685, usec = 320814 } Fri Nov 13 12:41:25 2026
admin            console      Nov 13 12:41 
<unknown>:0: error: error opening input file '/tmp/lk.swift' (No such file or directory)
12:52  up 11 mins, 1 user, load averages: 1.03 4.18 5.12
lanex2-l -> lanex2-m: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Manageds-Virtual-Machine.local
lanex2-w -> lanex2-m: runtime=ok(2s) | transport=ok(3s) | ssh=ok(3s) | exec=ok Manageds-Virtual-Machine.local
Paperboat daemon service status: running
laneX2-l
```

## macOS before the first login after boot (autologin switched off, reboot, nobody logs in)
```
== autologin off
2026-11-13 12:54:11.382 sysadminctl[6613:28784] SACSetAutoLoginPassword error:22
2026-11-13 12:54:11.389 defaults[6614:28789] 
The domain/default pair of (/Library/Preferences/com.apple.loginwindow, autoLoginUser) does not exist
/etc/kcpassword
reboot at 12:54:11Z
System shutdown time has arrived
== 60s after reboot: ssh to the Mac itself
12:55  up 52 secs, 0 users, load averages: 7.54 2.66 1.03
---
root             /System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow
root             /Library/PrivilegedHelperTools/io.paperboat.machineguard
Could not print domain: 125: Domain does not support specified action
368	0	com.pinksaucepasta.paperboat.carrier.u501.f166e63611209ac58265c155be9caff0
372	0	io.paperboat.machineguard
366	0	com.pinksaucepasta.paperboat.runtime-privileged.u501
512	1	com.pinksaucepasta.paperboat.hostd.u501.f166e63611209ac58265c155be9caff0
371	0	com.pinksaucepasta.paperboat.updated.u501
365	0	com.pinksaucepasta.paperboat.session-owner.u501.f166e63611209ac58265c155be9caff0
lanex2-l -> lanex2-m: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Manageds-Virtual-Machine.local
lanex2-w -> lanex2-m: runtime=ok(2s) | transport=ok(3s) | ssh=ok(3s) | exec=ok Manageds-Virtual-Machine.local
== service status via ssh on the Mac (no GUI session)
Paperboat daemon service status: stopped
Paperboat doctor: unhealthy
Correlation: support_fc7a61a0-1053-4dbb-b655-a6f76a122199
authentication: pass: The Paperboat server accepted the current credential.
clock: pass: This machine's clock matches the Paperboat server's.
daemon: pending: The local daemon is starting. Registering this computer with the Paperboat network failed: Paperboat account credentials are unavailable; retrying in 1s.
  Recovery: Paperboat is still starting; run `pb doctor` again in a moment.
daemon:register_network: fail: Registering this computer with the Paperboat network failed: Paperboat account credentials are unavailable; retrying in 1s
  Recovery: Paperboat retries this on its own. If it keeps failing, check this computer's network and that the Paperboat server is reachable, then run `pb doctor` again.
hostd: pass: The Paperboat host service answers.
local_names: pass: Local browser names resolve to this computer.
local_state: pass: Local Paperboat state and identity are valid.
machine_credentials: pass: The Paperboat server accepts this machine's credentials.
== 150s after reboot
lanex2-l -> lanex2-m: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Manageds-Virtual-Machine.local
lanex2-w -> lanex2-m: runtime=ok(3s) | transport=ok(5s) | ssh=ok(3s) | exec=ok Manageds-Virtual-Machine.local
== restore autologin 12:57:20Z
```
Autologin was restored and the VM rebooted again (ready 58 s after the command, admin logged in, service running).

## Windows (lanex2-w, victus), observed from M
```
## baseline
lanex2-m -> lanex2-w: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Victus
## RDP session opened (Pujan, interactive)
 SESSIONNAME               USERNAME                 ID  STATE   TYPE        DEVICE 
>services                                            0  Disc                        
 console                                             1  Conn                        
 rdp-tcp#0                 Pujan                     2  Active                      
 rdp-tcp                                         65536  Listen                      

lanex2-m -> lanex2-w: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Victus
pb service status (on W): Paperboat daemon service status: running
W exec M: Manageds-Virtual-Machine.local
## RDP disconnected (client closed, session stays)
Terminated
 SESSIONNAME               USERNAME                 ID  STATE   TYPE        DEVICE 
>services                                            0  Disc                        
 console                                             1  Conn                        
                           Pujan                     2  Disc                        
 rdp-tcp                                         65536  Listen                      

lanex2-m -> lanex2-w: runtime=ok(0s) | transport=ok(2s) | ssh=ok(1s) | exec=ok Victus
## logoff of that user
logoff session 2
 SESSIONNAME               USERNAME                 ID  STATE   TYPE        DEVICE 
>services                                            0  Disc                        
 console                                             1  Conn                        
 rdp-tcp                                         65536  Listen                      

lanex2-m -> lanex2-w: runtime=ok(0s) | transport=ok(2s) | ssh=ok(1s) | exec=ok Victus
pb service status (on W): Paperboat daemon service status: running
W exec M: Manageds-Virtual-Machine.local

# part A2: locked Windows session, 18:49:55Z
session:  rdp-tcp#0                 Pujan                     4  Active                      
LogonUI processes (a locked session shows one): 2
lanex2-m -> lanex2-w: runtime=ok(0s) | transport=ok(1s) | ssh=ok(1s) | exec=ok Victus
pb service status (on W): Paperboat daemon service status: running
W exec M: Manageds-Virtual-Machine.local
Terminated
logoff session 4
 SESSIONNAME               USERNAME                 ID  STATE   TYPE        DEVICE 
>services                                            0  Disc                        
 console                                             1  Conn                        
 rdp-tcp                                         65536  Listen                      
lanex2-m -> lanex2-w: runtime=ok(0s) | transport=ok(2s) | ssh=ok(1s) | exec=FAIL pb: Paperboat could not reach the target machine. It may be offline, or its network or Paperboat ser

# part A3: exec timeline across the logoff of an RDP user, 19:10:42Z
t+ 10s RDP session active exec=ok (10.2s)
Terminated
logoff session 6
t+ 18s logoff issued
t+ 27s after logoff exec=ok (9.6s)
t+ 40s after logoff exec=ok (9.7s)
t+ 53s after logoff exec=ok (9.7s)
t+ 66s after logoff exec=ok (9.8s)
t+ 78s after logoff exec=ok (9.8s)
t+ 91s after logoff exec=ok (9.7s)
t+104s after logoff exec=ok (9.9s)
t+117s after logoff exec=ok (9.9s)
t+130s after logoff exec=ok (9.8s)
t+143s after logoff exec=ok (9.9s)
t+155s after logoff exec=ok (9.7s)
t+168s after logoff exec=ok (9.8s)
t+181s after logoff exec=ok (9.6s)
t+193s after logoff exec=ok (9.8s)
```
Part 3b, restart with nobody signed in (earlier run, 21:40 IST): shutdown /r issued 16:10:50Z, victus ssh answered again after 432 s, pb ready from M 42 s after that (runtime/transport/ssh/exec ok), sessions after the boot: services and console only (no user), pb service status running; no user action.

## Not run / observations
- Windows hibernation: R-X-26 (40e072829) proved offline (allowed to sleep) then online 11 s after wake; not repeated. Fast startup: skipped (user rule: never shut victus down). Fast user switching on Windows: not producible remotely (one interactive session at a time, one account with credentials).
- macOS FileVault: the VM has it off, apple (real Mac, FileVault on) is the user's; not run. Linux asleep/offline reason and a re-run of logged-out/reboot on today's main with a cross-OS observer: hp down.
- OBSERVED (macOS): before the first login the machine is reachable (hostd, carrier, session-owner LaunchDaemons run): wait runtime/transport/ssh and exec from L and W ok at 60 s and 150 s. `pb service status` there says stopped (the per-user LaunchAgent is not loaded) and `pb doctor` says daemon pending, account credentials unavailable (login keychain locked); both are true of the user daemon, neither says the machine is reachable.
- OBSERVED (Windows): one exec from M failed right after a logoff (6 s after, runtime/transport/ssh ok, 'could not reach the target machine'), not reproduced in a 3.5 min exec timeline across a logoff (14 of 14 ok) nor in the two other logoffs.
- OBSERVED (Linux, part 1): `pb service status` as a logged-out user prints not-installed while the daemon serves; cause read in cli/internal/hostruntime/service/native_controller_unix.go:29-30 (bus unreachable maps to empty status), not changed.
