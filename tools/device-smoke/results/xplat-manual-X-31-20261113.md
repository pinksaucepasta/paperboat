# X-31 on release 2026.11.13.0 (built from d77a40713, contains bfd02e2e8; server .21 live), 2026-11-13

Machines (account A, prod server, all three on the official release installed by the printed commands): Linux container `x31b-l` (anvit, arm64) + a second Linux container as controller, macOS arm64 GitHub runner `macpool-1` (fresh), Windows GitHub runner `winpool-12` (fresh). `pb version` of each: 2026.11.13.0. Live `install.ps1` has the OS guard (served script contains "this command is for Windows").

Harness `tools/device-smoke/xplat_commands.py` (fixes made during this run: unique API token name per sign-in, Windows pb path refreshed after an earlier install, `pb uninstall` already revokes its own record, bounded wait for teardown, first-install of every pair source).

## wrong + dialog (xplat-commands-20261113T131511Z.json; its addjson/tunnel/install FAILs were the harness bugs above, rerun below)
PASS X-31:wrong:L the PowerShell command in pwsh stopped with "this command is for Windows", nothing left behind
PASS X-31:dialog:L 19/19, POSIX command pasted, Machine connected
PASS X-31:wrong:M the PowerShell command in pwsh (macOS) stopped with "this command is for Windows", nothing left behind
PASS X-31:dialog:M 19/19, POSIX command pasted on the fresh Mac, Machine connected
PASS X-31:wrong:W the POSIX command in Git bash stopped with "only macOS and Linux are supported", nothing left behind
PASS X-31:dialog:W 19/19, PowerShell command pasted, Machine connected

## install, all six directions (xplat-commands-20261113T151351Z.json)
PASS install L>M, L>W, M>L, M>W, W>L, W>M (the command printed by pb on A installed and paired B, online)

## addjson + tunnel on L, M, W (xplat-commands-20261113T151450Z.json)
PASS addjson L, M, W; PASS tunnel L, M, W (the printed `pb tunnel create` line runs verbatim)

Browser dialog in Edge on W and Chrome on M: 16/16 each, earlier transcript xplat-manual-X-31-20261112T200814Z.md (console unchanged since).
