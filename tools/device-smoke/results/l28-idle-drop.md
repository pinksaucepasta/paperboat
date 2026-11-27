# L28: idle web terminal drop, live check (2026-11-27)

Question: does the daemon fix stop the idle web terminal drop (before: ~25 s after attach, reconnect loop, data-terminal-close=connection_lost)?

## Builds

| build | commit | version string | used for |
|---|---|---|---|
| dev-87cd4d12d | 87cd4d12d (the fix as named in the task) | dev-87cd4d12d | Windows and Linux idle trials (shared pb-build.sh device build; linux/amd64, windows/amd64) |
| 2026.11.27.bd85dc59a | bd85dc59a (fix on main, per later coordinator rule) | 2026.11.27.bd85dc59a | fresh installs on Windows, Linux, macOS |

The bd85 build follows tools/build-release-asset.sh at bd85dc59a: CGO_ENABLED=0, -trimpath, -buildvcs=false, `-s -w`, Distribution=official, ProtocolVersion=1, DefaultServerURL and DefaultReleaseURL = https://api.pprbt.dev, mkwinres for Windows. **Deviation:** the script refuses to build without a valid SENTRY_DSN secret, which is not available here, so the build was made with an empty DefaultSentryDSN. Sentry release tag left unset in effect.
SHA-256 (bd85): linux/amd64 b776e7fd…; darwin/arm64 e2e27d02…; windows/amd64 f39b7122….

## Fresh install checks (rule: clean runner that never had pb)

| OS | runner | how clean | install | pair | ping | doctor | result |
|---|---|---|---|---|---|---|---|
| Linux | linuxpool-19 (machine_be4418b6, test-1127-l18lf) | VM up 56 min when claimed; no ~/.local/bin/pb, ~/.pb-pool, ~/.config/paperboat, units | PASS (65 s) | PASS | `pb ping test-1127-l18m` from here: 3/3 received, 0% loss, path regional_relay, rtt 141 ms | healthy | **PASS** |
| macOS | macpool-7 (machine_425fe5f4, test-1127-l18m) | up 22 min; no ~/.local/bin/pb, ~/.pb-pool, ~/.config/paperboat | PASS (78 s) | PASS | target of the ping above | healthy | **PASS** |
| Windows | winpool-10 (test-1127-l18wf, machine_c790340c) | new VM, booted 06:14 UTC; no C:\Program Files\Paperboat, no .pb-pool (a service-name match was DispBrokerDesktopSvc, a false positive) | copy OK, install-pair **FAIL** (40 s) | not paired | not run | not run | **FAIL** |

Windows failure, verbatim from the harness result (windows-lifecycle-20261127T062010Z.json, truncated by the harness):
`pair failed: {"code": "operation_failed", "category": "local_io", "message": "Windows installation failed while preparing updater state. prepare Windows updater for supplied install: worker release is revoked Run ...`
Stopped at install-pair while preparing the updater state. Not worked around. winpool-10 now holds a partial install and is down.

No second clean Windows runner was available (`winpool.sh start` reported 0 of 14 free), so the official build could not be compared with a dev build on a clean Windows host.

## Idle trials (Chromium headless unless stated; 0 reconnects, data-terminal-close empty, echo printed = pass)

| host | build | runs | reconnects | close attr | echo after idle | result |
|---|---|---|---|---|---|---|
| Windows winpool-7 (machine_d975afb4, test-1127-l18w-3) | dev-87cd4d12d | 90 s x5 | 0 x5 | empty x5 | PRINTED x5 | 5/5 pass |
| Windows winpool-7 | dev-87cd4d12d | 300 s x1 | not run: winpool-7 down at 14:20 (VM stopped) | | | not run |
| Windows winpool-7 | dev-87cd4d12d | WebKit | not run (same reason) | | | not run |
| Linux linuxpool-19 fresh (machine_be4418b6) | bd85 release-style | 90 s x5 | 0 x5 | empty x5 | PRINTED x5 | 5/5 pass |
| Linux linuxpool-19 fresh | bd85 | 300 s x1 | 0 | empty | PRINTED | pass |
| Linux linuxpool-19 fresh | bd85 | WebKit 90 s x2 | 0 x2 | empty x2 | PRINTED x2 | 2/2 pass |
| Linux linuxpool-8 (machine_4f44aff9, test-1127-l18l, first pb install on this runner, not fresh-verified) | dev-87cd4d12d | 90 s x5 | 0 x5 | empty x5 | PRINTED x5 | 5/5 pass (labelled: dev build, not the release-style check) |
| macOS macpool-7 fresh (machine_425fe5f4) | bd85 release-style | 90 s x2 (Chromium) | 0 x2 | empty x2 | PRINTED x2 | 2/2 pass |

Each run also recorded the socket/attribute logs in JSON (per trial) under the scratch run folders; attribute log shows only the initial attach (data-terminal-phase=attached, reconnects 0, resyncs 0).
Note: the page exposed no WebSocket or WebTransport objects to the page hook (ws=0 in every trial), so socket-level evidence is not available; the pass rests on the DOM attributes and the echo.

## Earlier attempts (kept for the record)
- Windows test-1127-l18w (machine_f7af834b, first paired on winpool-6 at 11:0x, winpool-6 then dropped off: ssh-monitor port 22206 refused, board down; winpool-6 freed). The same id was reused for a reinstall on winpool-7 at 11:44 but its runtime stayed offline; the live record was test-1127-l18w-3. A trial against machine_f7af failed at attach (90 s timeout), which matches the offline record.
- A browser run was stopped by a load check (load 19) and retried later; no run was started above load 12 otherwise.

## Cleanup
- Runners freed: linuxpool-8, linuxpool-19, winpool-7, winpool-10 (winpool.sh stop, started by this lane), macpool-7 (macpool.sh release, left running), winpool-6 (freed earlier, down).
- Machine records NOT revoked: `cleanup_test_account.py --id ... --apply` reported "revoked 0 machine(s)" for all seven records this lane paired (machine_4f44aff9, machine_be4418b6, machine_425fe5f4, machine_f7af834b, machine_9c14827e, machine_d975afb4, machine_c790340c); the same tool with `--prefix test-` also found 0 candidates, so the cause is not clear from here. The records remain on the account and need a manual revoke or the pool keeper's sweep.
- Scratch copies deleted (source tarball, harness copies, the release build on oracle and anvit). The dev-87cd4d12d device build cache on oracle was kept (shared cache keyed by commit).
- The Windows install on winpool-10 is partial (pair failed); nothing was hand-fixed.

## Windows fresh (2026-11-27, runner that never had pb)

Build: bd85dc59a, windows/amd64, version 2026.11.27.99 (four numeric parts). Built on linuxpool-5 with the flags of cli/tools/build-release-asset.sh at bd85dc59a. The script refuses without a SENTRY_DSN, so the build was run with the same flags and an empty DefaultSentryDSN; Commit=unknown (GITHUB_SHA unset). SHA-256 16b0b1184ccef854ec1f636a613bef0cc92cdf7b7eaa9c389c2433f1efc12b8e.

**Runner:** winpool-5 (fresh VM, started with `winpool.sh start 1 3`; board lock "L28 fresh windows"). Machine `test-1127-l28w`, record `machine_325115ee-6c90-47c6-a9b0-12be73a94003`.

**Clean evidence (before install, on winpool-5):** VM booted 2026-11-27 09:14 UTC; Windows Server 2025 Datacenter. No Paperboat service (`sc query` and Get-Service); `C:\ProgramData\Paperboat` and `C:\Program Files\Paperboat` absent; `AppData\Local\Paperboat` absent for Public and runneradmin; no `.pb-pool`, `.pb` or Roaming Paperboat; no `pb` on PATH; no `pb.exe` under Program Files, ProgramData or Users (depth 3); no Paperboat entry in the uninstall registry. The only service-name match was `DispBrokerDesktopSvc` (contains "pb"), a false positive.

**winpool-10 (earlier):** from the code, a version whose fourth part is not a number (`2026.11.27.bd85dc59a`) fails
`validVersion`, so `activeVersionPermitted` reports it as revoked (`workerupdate/tuf.go`, `manager.go`). The
Windows updater preparation only reaches that check when a Windows runtime config already exists
(`updated/native_install_windows.go`). Why one existed on winpool-10 is not shown: its clean checks found no earlier
install. The rerun used a four-number version, as releases do, and passed. The code the fix touches is not in this path.

**Install and pair (one run, no retry):** PASS, 115 s (`pool-install.sh`, `windows:install-pair` PASS 84 s, machine `machine_325115ee-6c90-47c6-a9b0-12be73a94003`, name `test-1127-l28w`). Install JSON and log: `receipts/install/test-1127-l28w.{json,log}`.

**Ping:** from anvit@hp (signed-in mint host): 4/4 received, path regional_relay, rtt 240.4 ms avg. From oracle: 0/4. Oracle's own pb is degraded (`pb doctor`: hostd not running, `machine_credentials` fail "the server no longer accepts this machine", `peer_paths` fail). Not touched; needs its own repair.

**Windows machine doctor:** `pb doctor` on the machine reported no fail or warning lines; `managed_ssh` pass; runtime ready.

**Idle trials** (`idle-trial.mjs`, one browser at a time, live console terminal, attach, idle, then echo):

| engine | idle | runs | reconnects | close attr | echo after idle | result |
|---|---|---|---|---|---|---|
| Chromium | 90 s | x5 | 0 x5 | empty x5 | printed x5 | 5/5 PASS |
| Chromium | 300 s | x1 | 0 | empty | printed | PASS |
| WebKit | 90 s | x2 | 0 x2 | empty x2 | printed x2 | 2/2 PASS |

Raw lines: `/home/ubuntu/paperboat-private/clean-smoke-1127/l28w/trials.log`.

**Cleanup:**
- Record revoked: `pb machine revoke` (preview, then `--confirm` on anvit@hp), "Disconnected machine test-1127-l28w". `pb machine list` no longer shows it. `cleanup_test_account.py --apply` again reported 0 revoked.
- winpool-5: `winpool.sh stop 5` refused (claim owner `/home/ubuntu/paperboat`, from `runner take`, not this worktree). The machine stays up. Board lock freed; board shows FREE. Needs a hand-back from the owner.
- linuxpool-5 (build host): freed after the build.
- Scratch: source tree and Windows binary deleted. Kept: `idle-trial.mjs`, `trials.log`, install receipts.
- Caution: `runner take linuxpool 30` took linuxpool-5 without an expiry note. Its board entry showed "L31: cli race suite" about 5 minutes earlier, and it was free at take time. The L31 lane may not have expected it to be released.
