# F1-12 official Windows one-liner on winpool-8 — FAIL (doctor degraded, install+pair OK)

Runner: `winpool-8` (fresh, unpaired, up since 17:45Z; no Paperboat before). SSH CWD `/home/ubuntu/.ssh`.
Source: official Add-machine dialog one-liner via mode-600 sink file (polled with stat only, used within ~1 min of creation; fed to the runner over ssh without echoing; stdout/stderr to mode-600 `/tmp/f1-12-win-install.log`, `p=` redacted).

## Install phase — PASS
- One-liner ssh exit code: **0**.
- Redacted log (4 lines, full):
  - `Installed pb to C:\Program Files\Paperboat\users\ue655ce475da61ffc2e916d1d\bin\pb.exe`
  - `Completing one-shot machine enrollment...`
  - `Requesting administrator approval to install Paperboat Windows services...`
  - `Paperboat Windows machine runtime is installed. It will resume after reboot.`
- No approval prompt or browser login blocked it (ran as Administrator, non-interactive).

## Pairing — PASS
- Installed version: **2026.11.10.4** (`pb --version`, `daemon_version` in status).
- Local machine (from `runtime-install.json` on runner): alias **runnervmm7ud1-2**, id `machine_e361adc0-ea3a-452d-9050-e759cde2e173`, workspace `C:\Users\runneradmin`, `enrollment_pending: false`, `installed_at: 2026-11-10T17:51:11Z`.
- `pb auth status --json`: `signed_in: true`, account id prefix `user_5ae46801-…`.
- `pb status`: daemon **ready**, generation 8; this machine `Runtime: ready` (online).

## Doctor — DEGRADED (the failing phase)
- `pb doctor` exit **1**, overall: `Paperboat doctor: degraded`.
- All checks pass except one warning: `daemon/browser_urls_unavailable: warning — Browser URLs are off because another program is using port 80/443 (Windows HTTP service (HTTP.sys, for example IIS), PID 4)`.
- Cause: hosted runner has HTTP.sys (IIS) bound to 80/443 — environmental, pre-existing, not created by the install. Product code: `cli/internal/localdaemon/inventory.go:323` (`browser_urls_unavailable`; busy-gateway-port case covered in `inventory_local_access_step_test.go:40`).

## Verdict: FAIL
Per the task criteria (exit 0 + paired + doctor healthy): install exit 0 ✓, paired ✓, doctor healthy ✗. Failing phase is post-install doctor health, caused by the runner's port 80/443 conflict above.

## End state
Left paired (no uninstall/reset): `runnervmm7ud1-2` (`machine_e361adc0-…`) online on winpool-8, daemon ready. Nothing else on the runner touched.
