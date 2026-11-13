# X-28 W part: a Windows OS user with a space and a non-ASCII letter ("Zoë Test") as a managed-SSH target, 2026-11-13 (UTC 14:20-15:00), lane R-X-28B

Build on every machine: dev-d023709c9 (main d023709c9 = 7ea096d37 + 8330c2092 + 84e6b29ee + others), `pb version` checked on each:
Linux container rx28b-l (dadape, arm64, user pbt), macOS arm64 macpool-3 (user runner), Windows Server 2025 (10.0.26100) winpool-7, pb run as OS user `Zoë Test`
(local administrator, profile C:\Users\Zoë Test). Server 2026.11.13.23 (migrations 307+308 applied). Account: the fleet account, every record named `rx28b-*`.
OpenSSH on the runner is the approved Microsoft.OpenSSH.Preview 10.0.0.0 package; the runner's admin account `runneradmin` paired first (rx28b-w) so Paperboat
provisioned it, and the second user's own PaperboatSshd service came up after that.

## What changed (commits 7ea096d37, 8330c2092, 84e6b29ee)
Server managedssh.ValidOSUser and CLI managedssh.ValidUsername accept any printable account name (Unicode letters, spaces, dots, hyphens, optional DOMAIN\user);
refused: control characters, leading '-', edge spaces, and `" ' ` $ % * , ? ! [ ] / @ ; | & < > ( ) { } ^ # = : + ~` (what OpenSSH patterns, its quoting or the
ProxyCommand shell "%r" cannot carry). Migrations 307/308 relax the machine_ssh_targets CHECK. The alias fragment capitalised a first byte; now a first rune.
Prior art: Win32-OpenSSH accepts quoted `Match User "name with space"`; checked here with `sshd -t` on the runner for plain, space, ë and the exact Paperboat
Match lines (rc=0 each).

## Results (machine, version, command, result)
```
W target  winpool-7 as `Zoë Test`  pb dev-d023709c9  pair rx28b-zoe -> machine online; server created the SSH target (os_user "Zoë Test", port 22)
          Get-Service: PaperboatSshd-uf8139163... Running
L client  rx28b-l pb dev-d023709c9: pb ssh doctor rx28b-zoe -> Local configuration ready, Managed agent ready, Transport ready, Target ready on port 43507, Managed keys reconciled
          alias block: Match host <alias> user "Zoë Test,zoë test,ZOË TEST,Zoë test"
          plain ssh -o BatchMode=yes "Zoë Test@<alias>.<local domain>" whoami -> runnervmm7ud1\zoë test
          pb ssh rx28b-zoe -- whoami -> runnervmm7ud1\zoë test
          plain ssh as another name (nobody@...) -> Permission denied (publickey,password)   [managed key is offered only for the registered user]
M client  macpool-3 pb dev-d023709c9: same alias block; pb ssh doctor Transport ready / Target ready / Managed keys reconciled;
          plain ssh "Zoë Test@<alias>" whoami -> runnervmm7ud1\zoë test;  pb ssh rx28b-zoe -- whoami -> runnervmm7ud1\zoë test
VS Code   server commit 69047191fc42 (the extension's own server) started through plain ssh from L on the Zoë target, PowerShell as remote shell:
          home=C:\Users\Zoë Test user=zoë test; unpacked under C:\Users\Zoë Test\.vscode-server\bin\<commit>; server port 63580; /version 69047191fc42...;
          task: task-ok C:\Users\Zoë Test; cleaned
```
Setup facts (not product bugs): a scheduled task for a user with a non-ASCII name needs COMPUTERNAME\name; the enrollment token file must be owned by the user with no Administrators entry.

## Open gap, not part of the row, not fixed
`pb doctor --repair` on this runner says "the repair did not finish" for EVERY user (admin `runneradmin`, plain `rxplain`, and `Zoë Test`), on a healthy dev-build install.
`pb.exe __runtime-service repair` elevated prints "invalid privileged installation request" (hostinstall.ErrInvalidRequest); so the logged symptom was not caused by
the deleted host-key files or by the user name. Commit 8330c2092 (EnsureHostKey on the repair path when the sshd service exists) closes the real gap that repair never
recreated deleted host keys, but it could not be checked on a device because repair fails earlier on dev builds. Origin of the ErrInvalidRequest not found (candidate:
install_windows.go reconcileWindowsRepairVersion, not confirmed; the services do point at the pinned custom path).
