# X-09 L<->W by hand (rx09), 2026-11-13 02:53 UTC
- x09-l: Ubuntu 24.04 container on pbvm2 (systemd user manager, linger), x09-w: Windows runner winpool-8; one account, repository rx09-config-fixture on pbvm2
- pb version: x09-l dev-aee62911e, x09-w dev-aee62911e
- host AppArmor userns restriction: 1

### baseline before apply
L: ['default.target.wants', 'paperboatd.service']
W: Name                                              Status StartType
----                                              ------ ---------
PaperboatRuntimeConfig-ue655ce475da61ffc2e916d1d Running Automatic

### `pb config sync apply` on x09-l
- state after apply: healthy
```
MainPID=3777 PrivateTmp=no ActiveState=active SubState=running 
PrivateTmp=false
1
```

### `pb config sync apply` on x09-w
- state after apply: healthy
```
Name                                              Status StartType
----                                              ------ ---------
PaperboatRuntimeConfig-ue655ce475da61ffc2e916d1d Running Automatic
```
- edit x09-l -> x09-w (after apply) arrived: PASS
- edit x09-w -> x09-l (after apply) arrived: PASS

### UPGRADE x09-l dev-aee62911e -> dev-e4d2cc0f8 (pb install of the new binary over it) while x09-w edits
- install: {"schema_version":"1.0","ok":true,"data":{"automatic_update_checks":false,"distribution":"custom","enrollment":"unchanged","executable":"/home/pbt/.local/bin/pb","sha256":"7cc214b98103aee8a9fbae3e75e1
- edit made on W during the upgrade arrived on L: PASS
- x09-l version dev-e4d2cc0f8, state healthy
```
MainPID=3777 PrivateTmp=no ActiveState=active SubState=running 
PrivateTmp=false
1
```
- edit x09-l -> x09-w (after upgrade L) arrived: PASS
- edit x09-w -> x09-l (after upgrade L) arrived: PASS

### REBOOT x09-l (docker restart of the systemd container) while x09-w edits
- edit made on W during the reboot arrived on L: PASS
- state after reboot: healthy
```
MainPID=157 PrivateTmp=no ActiveState=active SubState=running 
PrivateTmp=false
1
```
- edit x09-l -> x09-w (after reboot L) arrived: PASS
- edit x09-w -> x09-l (after reboot L) arrived: PASS

### UPGRADE x09-w dev-aee62911e -> dev-e4d2cc0f8
- installed dev-e4d2cc0f8
- x09-w version dev-e4d2cc0f8 state healthy
```
Name                                              Status StartType
----                                              ------ ---------
PaperboatRuntimeConfig-ue655ce475da61ffc2e916d1d Running Automatic
```
- edit x09-l -> x09-w (after upgrade W) arrived: PASS
- edit x09-w -> x09-l (after upgrade W) arrived: PASS

### UNASSIGN x09-l
- L state: disabled
```
MainPID=0 PrivateTmp=no ActiveState=inactive SubState=dead 
grep: /home/pbt/.config/systemd/user/paperboat-runtime-config.service: No such file or directory
1
```
- edit on W after L unassigned reached L: False (expected False)

### UNASSIGN x09-w
- W state: disabled
```

```

RESULT: sequence completed

### Windows logout (second pass, 03:17 UTC)
- `logoff 2` of the only interactive session (console, runneradmin) on the hosted runner winpool-8 ended the runner itself (ssh tunnel gone, host never answered again). A hosted runner cannot show logout or reboot of the worker; real Windows needs victus (reboot takes down pbvm2/3/4, the canary and the runner: coordinator decision).
- Linux "logout" not run: the container has no logind session to end; the restart above is the boot path.
- Notes: the config worker unit on Linux has `PrivateTmp=false` (host pbvm2 has apparmor_restrict_unprivileged_userns=1); MainPID stayed 3777 across the upgrade; after unassign the Linux unit file is removed and the Windows service is gone.
