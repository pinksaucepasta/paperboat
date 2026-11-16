# X-39 M: reboot of the fleet Mac (lanex2-m, pbmac Tart VM on a real Mac) with nobody logged in (lane FLEET4), 2026-11-16 07:40-07:55 IST

Build: lanex2-m installed dev-432169341 (= main a2742604e + 432169341, an Apple_Terminal status-bar change only) at 07:41 IST (exec 4 ways ok, doctor 0); lanex2-l2 and lanex2-w (observers) on dev-a2742604e. macOS 26, FileVault off (a VM), autologin user admin.
Steps (one Mac lock hold): autologin switched off (autoLoginUser deleted, /etc/kcpassword moved aside, both restored below); `tart set pbmac --memory 4096` (coordinator's request, applies at the next start; hw.memsize 6442450944 -> 4294967296); guest `shutdown -h now`; VM stopped after 22 s; started again by `tart run pbmac --no-graphics --vnc` on the host. The first start attempt (07:42:57, `setsid`, which macOS lacks) never started the VM: it sat stopped until I started it by hand at 07:51:46 IST; hence the "546 s after start" line in the log is the script's clock, the real boot was 07:51:46. VM ssh answered with uptime 19 s at 07:52:04.
Observers: lanex2-l2 and lanex2-w run `pb exec lanex2-m -- whoami` every ~15 s; the VM reports the console owner (`stat -f %Su /dev/console`) and `who | wc -l`.

Result PRELOGIN PASS: three consecutive observations (07:52:23, 07:52:37, 07:52:52 IST, 37-60 s after the VM started) with the console owned by root, 0 users logged in: L>M exec ok and W>M exec ok every time (only the LaunchDaemons running, as in part 3).
Then autologin restored (autoLoginUser admin, /etc/kcpassword back), guest reboot 07:53:12: console owner admin 30 s later, M version ok, exec M>W, M>L, L>M, W>M all rc 0, doctor rc 0, `pb service status` running, memsize 4294967296.
Not produced: a FileVault-locked boot (the VM cannot enable FileVault, the real Mac apple is the user's); recorded as an OS limit in part 4.

## Run log
```
07:39:54 M before ecbea1cad600fa6e5005b","version":"dev-a2742604e"}}
07:41:35 installed dev-432169341
M version 61cde6e85a71e7a9930bc","version":"dev-432169341"}}
M>W 0 Victus 
M>L 0 76e658e8b65b 
L>M 0 Manageds-Virtual-Machine.local 
W>M 0 Manageds-Virtual-Machine.local 
doctor M rc 0
m3rc=0
07:42:29 before: M 71e7a9930bc","version":"dev-432169341"}}
07:42:30 autologin off: 2026-11-16 02:12:30.051 defaults[57973:2651580]  | The domain/default pair of (/Library/Preferences/com.apple.loginwindow, autoLoginUser) does not exist | /etc/kcpassword.f4 | 6442450944
07:42:31 tart set: (0, '')
07:42:31 guest shutdown (halt) issued
07:42:54 VM state: B 42 GB 22 seconds ago stopped after 22s
07:42:57 tart run started: 
07:52:04 VM ssh answers 546s after start: 2:22  up 19 secs, 0 users, load averages: 11.90 2.69 0.96 | root |        0 | 4294967296 | mux note
07:52:23 L>M rc=0 admin | W>M rc=0 admin | console/users=root        0
07:52:37 L>M rc=0 admin | W>M rc=0 admin | console/users=root        0
07:52:52 L>M rc=0 admin | W>M rc=0 admin | console/users=root        0
07:52:52 PRELOGIN RESULT: PASS (3 good observations with nobody on the console)
07:52:52 autologin restored: admin | /etc/kcpassword
07:53:12 guest reboot issued (restore boot)
07:53:43 console owner after restore boot: admin (30s)
07:54:03 M version: 71e7a9930bc","version":"dev-432169341"}}
07:54:26 M>W rc=0 Victus
07:54:28 M>L rc=0 76e658e8b65b
07:54:30 L>M rc=0 Manageds-Virtual-Machine.local
07:54:32 W>M rc=0 Manageds-Virtual-Machine.local
07:54:33 doctor M rc=0; status: Paperboat daemon service status: running (machine services are running, remote access is served)
07:54:33 memsize now: 4294967296
m2rc=0
```
