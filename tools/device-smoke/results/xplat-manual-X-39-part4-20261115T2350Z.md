# X-39 part 4 (today's main dev-8c64c73e6, own machines, prod account), UTC 2026-11-15 23:10-23:50 (IST 04:40-05:20, 16 Nov)

Machines (all `pb version` = dev-8c64c73e6, main 8c64c73e6): r-x-39-n-l = Linux amd64 systemd container on pbvm2 (user pbt, linger on, sshd installed), r-x-39-n-w = hosted Windows Server runner winpool-7 (runneradmin console session, plus local users x39a/x39b for RDP sessions over `ssh -L`, xfreerdp3). Cross-OS observer: W observes L, L observes W. Times UTC.
Pair check on today's main: `pb exec r-x-39-n-l -- whoami` from W -> pbt; `pb exec r-x-39-n-w -- hostname` from L -> runner host.
Fixture note: the first `pb wait --for ssh` from W failed only because the container had no sshd ("Publishing the machine's SSH host keys ... start an SSH server (sshd)"); after installing openssh-server every `wait --for ssh` and `pb ssh r-x-39-n-l -- hostname` from W passes.

## Linux target, observed from W
### A logged out (Linger=yes): `loginctl terminate-user pbt`
## A logged out: Linger=yes; loginctl terminate-user pbt 23:48:10
sessions after: 0  user@1000: inactive
23:48:38 W wait runtime: r-x-39-n-l is ready for runtime (ready, generation 80).
23:48:41 W wait transport: r-x-39-n-l is ready for transport (ready, generation 80).
23:48:45 W wait ssh: r-x-39-n-l is ready for ssh (ready, generation 80).
exec=pbt ssh=7f17f7cfa4d5
pb service status as pbt: Paperboat daemon service status: running (machine services are running, remote access is served) 
### B reboot (docker restart of the systemd container, nobody logged in), with timeline
boot before: 2026-11-12 15:46:29 pb pids: 79 81 82 86 142 164 197 209 
restart returned 23:49:57
+12s exec=pbt
boot after: 2026-11-12 15:46:29 pb pids: 80 81 82 83 87 136 163 179  sessions: 0
23:50:11 W wait ssh: r-x-39-n-l is ready for ssh (ready, generation 80).
rc=0
(earlier run of B, same result: runtime/transport/ssh ready +5 s/+11 s/+14 s after the restart, exec and pb ssh ok, sessions: 0)

### C asleep / offline reason and wake (user-approved stand-in: SIGSTOP every pb process in the container + `docker network disconnect` for 5 min, then reconnect + SIGCONT). STATE column = `pb machine list --q r-x-39-n-l` on W
## baseline 23:27:07 state: online - machine_cd8d233d-8b39-4d10-af4b-9998d05acc48  | exec: pbt
## frozen pb pids 86 158 291 300 301 340 351 1072  + network cut 23:27:24
23:27:54 +30s state: online - machine_cd8d233d-8b39-4d10-af4b-9998d05acc48  | exec: pb: Paperboat could not reach the target machine. It may be offline, or its network or Paperboat service is not ready ye
23:28:41 +77s state: offline (allowed to sleep) | exec: pb: the machine is offline; start or repair Paperboat on it (`pb doctor`), then retry. Support reference: support_09cc2d
23:29:35 +131s state: offline (allowed to sleep) | exec: pb: the machine is offline; start or repair Paperboat on it (`pb doctor`), then retry. Support reference: support_30bff3
23:30:31 +187s state: offline (allowed to sleep) | exec: pb: the machine is offline; start or repair Paperboat on it (`pb doctor`), then retry. Support reference: support_c36ce2
23:31:26 +242s state: offline (allowed to sleep) | exec: pb: the machine is offline; start or repair Paperboat on it (`pb doctor`), then retry. Support reference: support_2d60b4
23:32:24 +300s state: offline (allowed to sleep) | exec: pb: the machine is offline; start or repair Paperboat on it (`pb doctor`), then retry. Support reference: support_d18ce8
## wake: network back + SIGCONT 23:32:49
23:32:49 W wait runtime: r-x-39-n-l is ready for runtime (ready, generation 57). after 11 s
23:33:00 state: online - machine_cd8d233d-8b39-4d10-af4b-9998d05acc48  | exec: pbt
rc=0

## Windows target, observed from L (winpool-7, today main). Phases: qwinsta sessions, exec, `pb wait` runtime/transport/ssh (18 of 18 ready, 0 failures); 6 exec during the 2 min after a logoff of both users
## baseline (console session runneradmin only) 23:40:44
 SESSIONNAME USERNAME ID STATE TYPE DEVICE 
 console runneradmin 2 Active 
exec=runnervmm7ud1 (5s)
  wait ssh: r-x-39-n-w is ready for ssh (ready, generation 26).
## RDP session x39a active 23:41:39
 SESSIONNAME USERNAME ID STATE TYPE DEVICE 
 console runneradmin 2 Active 
 rdp-tcp#0 x39a 3 Active 
exec=runnervmm7ud1 (3s)
  wait ssh: r-x-39-n-w is ready for ssh (ready, generation 28).
## fast user switching: x39a disconnected (session kept), x39b active 23:42:39
 SESSIONNAME USERNAME ID STATE TYPE DEVICE 
 console runneradmin 2 Active 
 x39a 3 Disc 
 rdp-tcp#0 x39b 4 Active 
exec=runnervmm7ud1 (6s)
  wait ssh: r-x-39-n-w is ready for ssh (ready, generation 28).
## switched back: x39b disconnected, x39a active 23:43:35
 SESSIONNAME USERNAME ID STATE TYPE DEVICE 
 console runneradmin 2 Active 
 rdp-tcp#0 x39a 3 Active 
 x39b 4 Disc 
exec=runnervmm7ud1 (4s)
  wait ssh: r-x-39-n-w is ready for ssh (ready, generation 28).
 
started
LogonUI processes: 1
## x39a session locked (LockWorkStation), x39b disconnected 23:44:05
 SESSIONNAME USERNAME ID STATE TYPE DEVICE 
 console runneradmin 2 Active 
 rdp-tcp#0 x39a 3 Active 
 x39b 4 Disc 
exec=runnervmm7ud1 (4s)
  wait ssh: r-x-39-n-w is ready for ssh (ready, generation 28).
logoff of both RDP users 23:44:33
t+20s  SESSIONNAME USERNAME ID STATE TYPE DEVICE | console runneradmin 2 Active | exec=runnervmm7ud1 (9s)
t+40s  SESSIONNAME USERNAME ID STATE TYPE DEVICE | console runneradmin 2 Active | exec=runnervmm7ud1 (4s)
t+60s  SESSIONNAME USERNAME ID STATE TYPE DEVICE | console runneradmin 2 Active | exec=runnervmm7ud1 (3s)
t+80s  SESSIONNAME USERNAME ID STATE TYPE DEVICE | console runneradmin 2 Active | exec=runnervmm7ud1 (5s)
t+100s  SESSIONNAME USERNAME ID STATE TYPE DEVICE | console runneradmin 2 Active | exec=runnervmm7ud1 (4s)
t+120s  SESSIONNAME USERNAME ID STATE TYPE DEVICE | console runneradmin 2 Active | exec=runnervmm7ud1 (4s)
## both users logged off (console runneradmin remains) 23:47:19
 SESSIONNAME USERNAME ID STATE TYPE DEVICE 
 console runneradmin 2 Active 
exec=runnervmm7ud1 (3s)
  wait ssh: r-x-39-n-w is ready for ssh (ready, generation 34).
rc=0

## macOS target, observed from W and L (macpool-6, hosted macOS 26.7.2 arm64, FileVault Off, user runner on the console, dev-8c64c73e6)
Fixture note: the hosted image shares its SSH host keys with other runners; pb ssh refuses ("host keys are identical to ..., cloned") until `sudo rm /etc/ssh/ssh_host_*; sudo ssh-keygen -A` and an sshd restart; after that `wait --for ssh` passes from W and L. That refusal is the intended product behaviour.

### screen locked (screen saver, password at once; session dictionary CGSSessionScreenIsLocked=1) and session off the console (fast user switching: SACSwitchToLoginWindow, kCGSSessionOnConsoleKey=0, loginwindow x2, gui/501 domain still alive)
## screen locked (screen saver, password required at once) 00:12:10: locked=1 onconsole=1 user=runner
W>M runner | L>M runner
  W wait ssh: r-x-39-n-m is ready for ssh (ready, generation 130).
  L wait ssh: r-x-39-n-m is ready for ssh (ready, generation 40).
t+20s W>M runner | L>M runner
t+40s W>M runner | L>M runner
t+60s W>M runner | L>M runner
t+80s W>M runner | L>M runner
## fast user switching: SACSwitchToLoginWindow, runner session off the console 00:14:35
00:14:54: locked=0 onconsole=0 user=runner
2 gui/501 = {        1 
t+20s W>M runner | L>M runner
t+40s W>M runner | L>M runner
t+60s W>M runner | L>M runner
t+80s W>M runner | L>M runner
t+100s W>M runner | L>M runner
t+120s W>M runner | L>M runner
  W wait ssh: r-x-39-n-m is ready for ssh (ready, generation 139).
  L wait ssh: r-x-39-n-m is ready for ssh (ready, generation 49).
ready lines (runtime, transport, ssh from both observers, two phases): 12

### nearest case to "before the first login after boot": the per-user LaunchAgent (paperboatd) booted out, only the LaunchDaemons (hostd, carrier, session-owner, machineguard; RunAtLoad/KeepAlive in /Library/LaunchDaemons) serve
## before: Paperboat daemon service status: running (machine services are running, remote access is served)
       0
0 agent booted out 00:19:11 
pb service status as runner: Paperboat daemon service status: stopped (machine services are running, remote access is served)
t+20s W>M runner | L>M runner
t+40s W>M runner | L>M runner
t+60s W>M runner | L>M runner
t+80s W>M runner | L>M runner
t+100s W>M runner | L>M runner
t+120s W>M runner | L>M runner
  W wait ssh: r-x-39-n-m is ready for ssh (ready, generation 143).
  L wait ssh: r-x-39-n-m is ready for ssh (ready, generation 52).
Paperboat daemon service status: running (machine services are running, remote access is served)

## Not producible on these machines (named gaps)
- Linux: none; every named part passed.
- macOS reboot with autologin, boot with nobody logged in, FileVault-locked boot: a hosted runner does not return from a reboot and the fleet Mac (lanex2-m) is still on dev-08d3c4462 (only T-FLEET2 may install a newer build). Last real reboot passes: dev-79203d6e5 (part 3). Five commits since touch Mac start-up code (6ff5db13e PF policy, 900bff252, 8b6045ecd/f79da3b52 hostd worker watchdog, eca5fb1ef Keychain bound), so the reboot parts need one re-run on a build with them. FileVault: with FileVault on, the data volume stays locked until a user enters the password at boot, so no LaunchDaemon from it runs; OS limit (no Tailscale/NetBird/Teleport source in ~/ref mentions it), nothing Paperboat can change; the machine shows offline and is reachable after the unlock. The VM and the runner have FileVault off, so not shown.
- Windows reboot with nobody signed in and fast startup: a hosted runner cannot reboot back, `powercfg /a` on it says "Fast Startup: Hibernation is not available" (hypervisor, HiberbootEnabled=0), victus must stay on (user rule, no more hibernate/shutdown tests). Last real passes: reboot dev-cf0c0a620 (part 3), hibernation R-X-26 (40e072829). Five Windows commits since touch service start-up (0156f8a5a, 8b6045ecd, 42172c232, 49aedcff2, dda3519a0), so the reboot part needs one `shutdown /r` of victus in a power window on a build with them.
