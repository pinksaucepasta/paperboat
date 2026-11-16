# X-09 W: in-place upgrade on victus with the config worker running (lane FLEET4), 2026-11-16 06:40-06:50 IST

Fleet: lanex2-w (victus, the real Windows PC) and partner lanex2-l2 (Linux container on anvit, dev-08d3c4462). Repository fixture on anvit (bare git + HTTP server, token), reached from victus through an ssh -R tunnel; scratch script (not committed) driving tools/device-smoke/xplat_config.py: Context.start/assign/finish, run under fleet-configsync.lock, config-sync.toml of both machines backed up and restored (neither had one).
Upgrade: victus dev-18de3c2f5 (pb install of that build, then config sync assigned on both) -> `pb install` of dev-a2742604e (today's main, no client change since; origin/main a2742604e..98f0d4e65 is console only), scp of the exe then `pb-xplat.exe install --json`, took 95 s. Edit C was written on L 10 s into the install (install still running) and arrived on W 2 s after the install returned.

Checks (all PASS): apply -> healthy on both before; edits both ways before; edit on the partner during the upgrade arrives after it; worker (PaperboatRuntimeConfig service) Running after the install with nobody touching it; edits both ways after (W->L, L->W); `pb config status` healthy/healthy on both after; pb version W dev-a2742604e; every Paperboat service and every pb process runs from the new version dir only.
Versions on disk after: the new version dir and ONE previous version dir (the one the install replaced). That is the signed rollback slot by design (hostinstall/install_windows.go RollbackSource; pin_gc_windows.go:82 keeps Source and RollbackSource, the next install replaces it), nothing runs from it; the older leftover that existed before this run was removed by the upgrade.
An earlier attempt (06:23 IST) failed in the harness only: scp to victus has no password (fleet.md); fixed in the scratch script with sshpass. A first complete pass on the same build (06:28-06:38 IST, from dev-08d3c4462) gave the same results; its status print was truncated, hence this rerun.

## Run log (second run, config status printed as (alias, state, manifest_health))
```
06:40:12 backed up sync files: {'L': False, 'W': False}
06:41:39 pre-step: installed dev-18de3c2f5 on W
06:42:19 pb version L=9977b2ec6e336639061ca50b8ac9bf2","version":"dev-08d3c4462"}} W=e38025d4ac3992668e2f465cf52a28b","version":"dev-18de3c2f5"}}
06:43:29 baseline L->W PASS
06:45:10 baseline W->L PASS
06:45:12 lanex2-w config status before: [('lanex2-l2', 'healthy', 'healthy'), ('lanex2-w', 'healthy', 'healthy')]
06:45:13 lanex2-l2 config status before: [('lanex2-l2', 'healthy', 'healthy'), ('lanex2-w', 'healthy', 'healthy')]
06:45:16 W state before upgrade:
PaperboatLocalDaemon-uba6b Running custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe"
PaperboatUpdated-uba6bceb9 Running custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe"
PaperboatSessionOwner-uba6 Running custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe"
PaperboatSshd-uba6bceb9290 Running custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe"
PaperboatCarrier-uba6bceb9 Running custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe"
PaperboatHostd-uba6bceb929 Running custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe"
PaperboatRuntimeConfig-uba Running "C:\Program Files\Paperboat\users\uba6bceb9290ebd807a30e102\bin\pb.exe"
VERSIONDIRS:
custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee
custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e
PROCS:
C:\Program Files\Paperboat\MachineGuard\pb.exe
C:\Program Files\Paperboat\users\uba6bceb9290ebd807a30e102\bin\pb.exe
custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e\pb.exe
06:45:26 edit C written on L during upgrade, install alive=True, +10s
06:46:51 install finished after 95s: {"automatic_update_checks": false, "distribution": "custom", "enrollment": "unchanged", "executable": "C:\Program Files\\Paperboat\\users\\uba6bceb9290ebd807a30e102\\bin\\pb.exe", "sha256": "4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee", "version": "dev-a2742604e"}
06:46:54 edit during upgrade arrived on W PASS (+2s after install)
06:48:10 W->L after upgrade PASS
06:49:53 L->W after upgrade PASS
06:49:55 lanex2-w config status after: [('lanex2-l2', 'healthy', 'healthy'), ('lanex2-w', 'healthy', 'healthy')]
06:49:56 lanex2-l2 config status after: [('lanex2-l2', 'healthy', 'healthy'), ('lanex2-w', 'healthy', 'healthy')]
06:49:59 W state after upgrade:
PaperboatLocalDaemon-uba6b Running custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe"
PaperboatUpdated-uba6bceb9 Running custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe"
PaperboatRuntimeConfig-uba Running "C:\Program Files\Paperboat\users\uba6bceb9290ebd807a30e102\bin\pb.exe"
PaperboatSessionOwner-uba6 Running custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe"
PaperboatSshd-uba6bceb9290 Running custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe"
PaperboatCarrier-uba6bceb9 Running custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe"
PaperboatHostd-uba6bceb929 Running custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe"
VERSIONDIRS:
custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee
custom-8d0518b1b1e6c595e2ae644fc1b31a08131769beb59b42b5e7b9dcd4f4189c7e
PROCS:
C:\Program Files\Paperboat\MachineGuard\pb.exe
C:\Program Files\Paperboat\users\uba6bceb9290ebd807a30e102\bin\pb.exe
custom-4aed451d737dd38e81fa0efb25f5696d69c6b6581fd4baf766fd257244e939ee\pb.exe
06:50:00 pb version W after: {"commit":"a2742604e879a5a8d05ecbea1cad600fa6e5005b","version":"dev-a2742604e"}}
06:50:26 restored sync files; RESULT PASS
rc=0
```
