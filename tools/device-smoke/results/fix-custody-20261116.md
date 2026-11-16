# ENV vault custody survives a daemon replacement, 2026-11-16 (IST 16:00-17:45)

Fix: the process that replaces the user daemon exports the unlocked vault custody from the old daemon over
the owner-only local API and imports it into the new one (memory only, listener UID checked).
Check command: `pb env list` (it needs the vault keys; `pb env run` reads host delivery and never needed them).
Control: a restart that bypasses the handoff must leave the vault locked, proving the handoff kept it.

| Machine | Build | Step | Result |
|---|---|---|---|
| Linux, hp container `custody-l` (Ubuntu 24.04, headless, no Secret Service) | dev-f8e8ed1f9 | unlock, `pb daemon __update-probe --restart` (the restart `pb update` runs), daemon pid 2326 -> 2613 | `pb env list` rc 0 without unlock (PASS) |
| same | dev-f8e8ed1f9 -> dev-4ed6c06b5 | unlock, `pb install` of the next build in place, daemon pid 2674 -> 4180, `pb version` dev-4ed6c06b5 | `pb env list` rc 0 without unlock (PASS) |
| same | dev-f8e8ed1f9 | control: `systemctl --user restart paperboatd` | "ENV vault is locked; run `pb env unlock`", rc 1 (expected) |
| macOS 26.6.2, pbmac2, own user `custodypb` (no GUI login, no keychain) | dev-c7d69ee06 | unlock, `pb daemon __update-probe --restart`, launchd pid 57033 -> 57866 | `pb env list` rc 0 without unlock (PASS) |
| same | dev-c7d69ee06 | control: `launchctl kickstart -k user/<uid>/com.pinksaucepasta.paperboatd`, pid -> 57997 | locked, rc 1 (expected) |
| Windows Server, winpool-1 `custody-w` | dev-f8e8ed1f9 -> dev-4ed6c06b5 | unlock, `pb install` in place (daemon processes restarted 11:34:26 UTC) | `pb env list` rc 0 (PASS) |
| same | dev-4ed6c06b5 | `pb service stop` + `pb service start` | still unlocked, rc 0: Windows keeps the keys in Credential Manager (`CredentialStoreAvailable` is always true there), so its daemon never held them and needs no handoff |

Tests: TestVaultCustodyHandoffMovesCustodyToReplacementDaemon, TestVaultCustodyRequiresExactOwnerBeforeReadingSecretBody,
TestHandOverVaultCustodySurvivesDaemonReplacement, TestHandOverVaultCustodyFailureLeavesNewDaemonLocked, TestCLIReference
(-race on hermes, commit c7d69ee06); GitHub ci-test cli run 43849168484 (same code on the earlier base).

## Two-machine check on the pushed build dev-c7d69ee06
hp container `custody-pl` (Linux) and a second Windows user `custodypb` on victus (`custody-pw`), both dev-c7d69ee06:
`pb exec custody-pw -- hostname` from Linux 3/3 rc 0 (817, 334, 576 ms); `pb exec custody-pl -- hostname` from Windows 3/3 rc 0
(1301, 520, 551 ms); `pb connect custody-pw new` from Linux in a pty echoed the typed marker and exited rc 0 (PASS).

Cleanup: records custody-l, custody-w, custody-pl revoked, custody-m and custody-pw removed by `pb uninstall`; hp containers removed;
pbmac2 user custodypb deleted; victus user custodypb deleted (C:\Users\custodypb keeps 16 files of a loaded registry hive until the next reboot).
