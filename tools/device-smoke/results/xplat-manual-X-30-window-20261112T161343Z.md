# X-30 by hand on the shared fleet (lanex2), disruptive window, 20261112T161343Z (UTC)

Fleet build dev-6c7e29534 on all three (`pb version`), doctor healthy before and after every OS. One OS at a time; each restored fully (daemon running, network on, logged in) before the next. Earlier, non-disruptive parts of the row (hook install/uninstall, plain ssh ENV equality for all six pairs, VS Code server, locked vault) are in results/xplat-manual-X-30-20261112T155643Z.md (build dev-be7f349ca).

Shell start = a new interactive shell that loads the hook (`bash -ic true` on L and M, `zsh -ic true` on M, `powershell -NoLogo` and `pwsh` with their profiles on W); output = bytes written to stdout+stderr.

## lanex2-l (Linux container): stopped daemon = `pb service stop`; no network = docker network disconnect; no login = PAPERBOAT_CONFIG pointing nowhere; no runtime = PAPERBOAT_RUNTIME_STATE_ROOT pointing nowhere
```
baseline 1: shell start 70 ms, output bytes 0
baseline 2: shell start 48 ms, output bytes 0
no pb login (no config/session): shell start 59 ms, output bytes 0
no runtime (state root missing): shell start 52 ms, output bytes 0
service: Paperboat daemon service status: stopped
daemon stopped: shell start 52 ms, output bytes 0
doctor: healthy
network cut: shell start 55 ms, output bytes 0
doctor: healthy
```
## lanex2-m (macOS runner): network cut = default interface down for ~6 s
```
baseline 1 [bash -ic]: shell start 114 ms, output bytes 0
baseline 1 [zsh -ic]: shell start 143 ms, output bytes 0
baseline 2 [bash -ic]: shell start 101 ms, output bytes 0
baseline 2 [zsh -ic]: shell start 79 ms, output bytes 0
no pb login [bash -ic]: shell start 124 ms, output bytes 0
no pb login [zsh -ic]: shell start 114 ms, output bytes 0
no runtime [bash -ic]: shell start 121 ms, output bytes 0
no runtime [zsh -ic]: shell start 103 ms, output bytes 0
service: Paperboat daemon service status: stopped
daemon stopped [bash -ic]: shell start 91 ms, output bytes 0
daemon stopped [zsh -ic]: shell start 79 ms, output bytes 0
doctor: healthy
network cut (interface en0 down) [bash -ic]: shell start 106 ms, output bytes 0
network cut (interface en0 down) [zsh -ic]: shell start 83 ms, output bytes 0
doctor: healthy
```
## lanex2-w (Windows runner): network cut = outbound firewall block rule (run as a SYSTEM scheduled task because the block ends the ssh session), removed afterwards
```
pb: ▄▄▝▀▀▀▀▀▘▄▄   Version dev-6c7e29534
baseline 1 [powershell with profile]: shell start 2018 ms, output bytes 0
baseline 1 [pwsh with profile]: shell start 977 ms, output bytes 0
baseline 2 [powershell with profile]: shell start 508 ms, output bytes 0
baseline 2 [pwsh with profile]: shell start 510 ms, output bytes 0
no pb login [powershell with profile]: shell start 499 ms, output bytes 0
no pb login [pwsh with profile]: shell start 485 ms, output bytes 0
no runtime [powershell with profile]: shell start 489 ms, output bytes 0
no runtime [pwsh with profile]: shell start 509 ms, output bytes 0
service: Paperboat daemon service status: stopped
daemon stopped [powershell with profile]: shell start 535 ms, output bytes 0
daemon stopped [pwsh with profile]: shell start 599 ms, output bytes 0
doctor after start: healthy
Connection to 127.0.0.1 closed by remote host.
network cut (outbound firewall block) [powershell with profile]: shell start 170 ms, output bytes 0
network cut (outbound firewall block) [pwsh with profile]: shell start 270 ms, output bytes 0
done
```
After the window: Windows doctor healthy, firewall rule gone, scheduled task and files removed, lanex2-w reachable from lanex2-l (`pb exec`).

## CRLF proof on Windows (build dev-6c7e29534 contains 9ab03e140): a profile written with BOM + CRLF
```
C:\Users\runneradmin\Documents\WindowsPowerShell\profile.ps1 install x2: hook lines=1 BOM=True lone LF=0
tail hex: 68 6F 6F 6B 0D 0A
C:\Users\runneradmin\Documents\PowerShell\profile.ps1 install x2: hook lines=1 BOM=True lone LF=0
tail hex: 68 6F 6F 6B 0D 0A
C:\Users\runneradmin\Documents\WindowsPowerShell\profile.ps1 after uninstall identical to the user's file=True
C:\Users\runneradmin\Documents\PowerShell\profile.ps1 after uninstall identical to the user's file=True
status: Paperboat ENV hook: in C:\Users\runneradmin\Documents\WindowsPowerShell\profile.ps1, C:\Users\runneradmin\Documents\PowerShell\profile.ps1, workspace personal. 11 values load.
```
Both PowerShell profiles: one hook line after two installs, BOM kept, no lone LF (the hook line ends 0D 0A), uninstall returns the user's bytes. (On dev-be7f349ca the same test left a bare 0A.)
