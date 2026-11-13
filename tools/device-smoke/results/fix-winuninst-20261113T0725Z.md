# FIX-WINUNINST: Windows uninstall leaves old pb.exe daemons and %LOCALAPPDATA%\Paperboat\credentials

Runner winpool-8 (Windows Server 2025), builds dev-e4d2cc0f8 -> dev-d63bbdcf5 -> dev-fdfe1baad (= `pb version` after each step),
driven with tools/device-smoke/windows_lifecycle.py (install-pair, update, update, uninstall) and `pb uninstall` by hand.

| Step | Result |
|---|---|
| install-pair e4d2cc0f8, update d63bbdcf5, update fdfe1baad | PASS, one generation left (12 pb.exe: MachineGuard, bin\pb.exe daemon, 10 runtime children, all one versions\ dir) |
| `pb uninstall` (by hand) | status.json `completed`; 0 pb.exe; no Paperboat services; C:\Program Files\Paperboat, C:\ProgramData\Paperboat and %LOCALAPPDATA%\Paperboat gone (credentials\*.dpapi, profiles\*.json existed before) |
| `pb install` right after `pb uninstall` | refused while the helper runs ("Paperboat is still being removed"), no race |
| install-pair, then harness `uninstall` (inventory check) | PASS; 0 pb.exe, credentials gone, no services |

Not reproduced on main 2cb3bc01c. No hostinstall/uninstall/credential code changed between the failing build (35ff839a8) and now.
The failing machine (winpool-4) held a previous lane's install: orphans that run from a layout or instance the current build
did not create are old state; AGENTS.md "zero compatibility" says such a host is reinstalled, not cleaned by new code.
Results: windows-lifecycle-20261113T071526Z, T071630Z, T071730Z, T072042Z, T072315Z, T072352Z (.json).

FIX-WINSWEEP follow-up (reading claim: the account sweep omits C:\ProgramData\Paperboat\updated\users\<inst>): not a bug.
purgeAccountSweepKeepingCLI removes layout.UpdateStateRoot (= updated\users\<inst>) in its removal loop, accountLeftoversKeepingCLI
re-scans it, and purgeWindowsSharedResidue / deleteWindowsSharedParentsAtRestart retire updated\users and updated
(covered by purge_shared_native_windows_test.go). Only the process-termination roots omit it; it holds tuf/journal data, no images.
The run above (install, two updates, uninstall on winpool-8) is the requested device check: ProgramData\Paperboat gone.
