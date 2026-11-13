# X-09 M part, 2026-11-13 12:50 UTC (18:20 IST)
- fleet: lanex2-l (Linux container on hp, Ubuntu with systemd user manager), lanex2-m (macOS pbmac VM, user admin, GUI session), lanex2-w (real Windows PC victus)
- pb version lanex2-l: dev-79203d6e5
- pb version lanex2-m: dev-79203d6e5
- pb version lanex2-w: dev-cf0c0a620

### `pb config sync apply` on L, M, W (one shared repository, bidirectional)
- lanex2-l state after apply: healthy
- **apply -> healthy on lanex2-l**: PASS 
```
MainPID=398054 PrivateTmp=no ActiveState=active SubState=running PrivateTmp=false
apparmor_userns=1
```
- lanex2-m state after apply: healthy
- **apply -> healthy on lanex2-m**: PASS 
```
5203	0	com.pinksaucepasta.paperboat.runtime-config
570	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboat.runtime-config.plist
com.pinksaucepasta.paperboatd.plist
gui_user=501
```
- lanex2-w state after apply: healthy
- **apply -> healthy on lanex2-w**: PASS 
```
Running  PaperboatRuntim... PaperboatRuntimeConfig-uba6bceb9290...
```
- **afterapplyM: edits of lanex2-l+lanex2-w arrived on lanex2-m**: PASS 
- **afterapplyM: edit made on lanex2-m arrived on lanex2-l**: PASS 
- **afterapplyM: edit made on lanex2-m arrived on lanex2-w**: PASS 

### LOGOUT of the macOS user session (launchctl bootout gui/501) while L and W edit
- bootout: 2
```
-	0	com.pinksaucepasta.paperboat.runtime-config
-	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboat.runtime-config.plist
com.pinksaucepasta.paperboatd.plist
gui_user=501
```
- **after logout the user agents are not running**: PASS on the output above (both agents listed with PID "-" = loaded but stopped); the script FAILED it only because its check looked for the label being absent. Check bug, not a product result.

### REBOOT of the macOS VM (auto-login brings the session and the agents back); L and W edit during it
- **machine rebooted and pb answers again**: PASS ({ sec = 1794574459, usec = 3 -> { sec = 1794574649, usec = 0)
```
573	0	com.pinksaucepasta.paperboat.runtime-config
850	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboat.runtime-config.plist
com.pinksaucepasta.paperboatd.plist
gui_user=501
```
- lanex2-m state after reboot: healthy
- **reboot -> healthy on lanex2-m**: PASS 
- exec lanex2-l -> lanex2-m rc=0 Manageds-Virtual-Machine.local
- exec lanex2-w -> lanex2-m rc=0 Manageds-Virtual-Machine.local
- exec lanex2-m -> lanex2-l rc=0
- exec lanex2-m -> lanex2-w rc=0
- **afterrebootM: edits of lanex2-l+lanex2-w arrived on lanex2-m**: PASS 
- **afterrebootM: edit made on lanex2-m arrived on lanex2-l**: PASS 
- **afterrebootM: edit made on lanex2-m arrived on lanex2-w**: PASS 

### UNASSIGN on lanex2-m
- lanex2-m state after unassign: disabled
```
598	0	com.pinksaucepasta.paperboatd
com.pinksaucepasta.paperboatd.plist
gui_user=501
```
- **unassign removes the worker on lanex2-m**: PASS worker output above
- **an edit on lanex2-l after unassign did not reach lanex2-m**: PASS 

### Notes
- Logout here is `launchctl bootout gui/501` (what the end of a GUI session does to a user's agents); a real loginwindow logout/login was not driven. The real login came from the reboot (auto-login), after which both agents ran again and the edits made during logout and reboot arrived.
- Not done on the Mac in this file: in-place upgrade while the worker runs (only T-FLEET2 installs on the fleet; no free macpool Mac).
- fleet builds: L and M dev-79203d6e5 (today's main is d77a40713), W dev-cf0c0a620.
