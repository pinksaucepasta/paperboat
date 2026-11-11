# victus clean install + pair + local-access, origin/main f11df5a69

victus-clean: PASS victus-clean-1 machine_5a014f31-fda5-4f9b-89f0-0f5ad7f8e93f
local access: healthy 12/12 minutes (doctor overall healthy every minute, daemon pass 12/12, no local_access_unavailable flapping)

Worktree read-only (no repo files touched). Raw: `/home/ubuntu/pb-wt/spark/results-victus-clean/`
(doctors/ has the 12 per-minute doctor JSONs). No VMs touched; `victus`/`victus-2`/`hp`/`apple`
records untouched. Own record `victus-clean-1` left paired by design. No secrets in this
report or the raw dir beyond the mode-600 enrollment-command JSON (token shown only as lengths);
server domains written as example.test.

## 1. Pre-state: clean as designed

- 0 Paperboat product services (only the GitHub runner); `Get-Service Paperboat*` empty.
  `C:\ProgramData\Paperboat` and `C:\Program Files\Paperboat` absent; `where.exe pb.exe`
  not found; C: free 45848616960 B. Only `%APPDATA%\Paperboat\replaces-machine.json`
  present, naming revoked machine_233d02bc (left alone). (`01-prestate.txt`,
  `services-prestate.txt`, `replaces-machine.json`)
- `icacls C:\ProgramData` (parent only): SYSTEM:(OI)(CI)(F), Administrators:(OI)(CI)(F),
  CREATOR OWNER:(OI)(CI)(IO)(F), Users:(OI)(CI)(RX) + (CI)(WD,AD,WEA,WA).
- Session elevated: `whoami /groups` shows Mandatory Label\High Mandatory Level (S-1-16-12288).

## 2. Build origin/main f11df5a69 on anvit

- `origin/main` IS `f11df5a69`; `merge-base --is-ancestor` passes (rc=0) for bf328c322
  (fresh-install owner fix), 0f3b5c513 (elevation leftover), eee5d7429 (W18 WFP batching),
  ce0a6aa41 (idle-CPU).
- Synced `git archive origin/main` (minus console/marketing paths) to
  `anvit:~/pb-main-build/f11df5a69...`, built via `~/bin/anvit-slot.sh` with AGENTS.md
  ldflags (`Version=dev-f11df5a69`, full commit, `ProtocolVersion=1`, Server/Release
  `https://api.example.test`), `CGO_ENABLED=0`: `pb-windows-amd64.exe` PE32+ x86-64
  sha `47ec2bf9…`, `pb-linux-amd64` ELF x86-64 statically linked sha `1ecdc914…`
  (`02-build-hashes.txt`). First linux build came out arm64 (host default); rebuilt with
  `GOOS=linux GOARCH=amd64`. Victus `Get-FileHash` matches the Windows build uppercase.
- Staged exe to `C:\Users\Pujan\pb-clean.exe` (since removed).

## 3. Install ok

- `pb-clean.exe install --json`: ok true, 18.92 s, version dev-f11df5a69, sha matches build,
  executable under `C:\Program Files\Paperboat\users\uba6bceb…\bin\pb.exe`. (`03-install-output.txt`)
- Right after: `icacls C:\ProgramData\Paperboat` = SYSTEM:(OI)(CI)(F),
  Administrators:(OI)(CI)(F), Users:(OI)(CI)(RX); `services` subdir = SYSTEM:(OI)(CI)(F),
  Administrators:(OI)(CI)(F); `(Get-Acl …).Owner` = NT AUTHORITY\SYSTEM — the bf328c322
  Administrators-custody shape, no refusal. (`04-acls-postinstall.txt`)

## 4. Token + pair: FIRST-TRY SUCCESS, no retry

- hp `/tmp/pb-clean-linux machine add --name victus-clean-1 --json` → command-shaped JSON
  (suffix token length verified 26 by length only, never printed; `05-token.json` mode 600).
  Staged suffix to `C:\Users\Pujan\pbtok-clean.txt` via `icacls /setowner Pujan` then
  `/inheritance:r /grant:r Pujan:F SYSTEM:F`; on-device ACL `VICTUS\Pujan:(F)`,
  `NT AUTHORITY\SYSTEM:(F)`, length 26. (`06-token-stage.txt`)
- `pair --enrollment-token-file … --name victus-clean-1 --json`: ok true, 134.30 s,
  record machine_5a014f31-fda5-4f9b-89f0-0f5ad7f8e93f. (`07-pair-output.txt`)
- First install+pair of the current era on this real hardware: NO install_readiness,
  carrier/readiness, or 'refused' failure, so no tokenless resume was run and no line was
  appended to `install-readiness-retries.log` (that log is one line per failed attempt).
  No `*-error.log` under `%LOCALAPPDATA%\Paperboat\runtime\` and no
  `PaperboatInstall-*-startup-error.log`. (`17-errorlogs-check.txt`)

## 5. Local access: healthy 12/12, listeners present, TcpTestSucceeded 3/3

- `doctor --json` every ~60 s x12 after pair: overall healthy 12/12; all 9 checks pass each
  time (authentication, clock, daemon, local_state, machine_credentials,
  machine_guard_version, peer_paths, udp_ipv4, udp_ipv6). The `daemon:local_access_unavailable`,
  `daemon:machine_services`, `daemon:browser_urls_unavailable` sub-checks never appear and
  never flap; `daemon` itself is pass 12/12. This build's doctor schema has no separate
  machine_services timing to record (absent, N/A). (`doctors/doctor-01..12.json`)
- `Get-NetTCPConnection -State Listen` for pb PIDs: many `127.100.*` native listeners
  (e.g. 127.100.0.1:38080, 127.100.0.30:59304, 127.100.0.1:59304) plus 127.100.0.1:80/443.
  (`10-listeners.txt`)
- `Test-NetConnection 127.100.0.1 -Port 38080`, `127.100.0.30:59304`, `127.100.0.1:59304`:
  TcpTestSucceeded True 3/3. (`11-nettest-reserv.txt`)
- `MachineGuard\reservations.json`: 18 claims, 56 names (peer-rich account as expected).
  (`11/12-reserv*.txt`)

## 6. Exec both ways, services, online; cleaned staging, left paired

- `hp → victus-clean-1 exec -- hostname` = Victus rc=0 (90 s patience, first syntax without
  `--` rejected, retried with `--`). `victus-clean-1 → hp exec -- hostname` = hp rc=0.
  (`13-exec-hp-to-victus.txt`, `14-exec-victus-to-hp.txt`)
- 6/6 product services Running (Carrier, Hostd, LocalDaemon, SessionOwner, Sshd, Updated).
  Record `victus-clean-1` state online, online True. (`15-services-final.txt`,
  `16-machine-list-final.json`)
- Staging removed on victus (`pb-clean.exe`, `pbtok-clean.txt` both gone), hp
  (`/tmp/pb-clean-linux`), local/anvit `/tmp` binaries+tarball. Installed product and
  pairing left intact and healthy: no repair or restart performed.
