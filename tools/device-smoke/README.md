# Device smoke

## Extra Windows machines (winpool)

`winpool.sh` turns up to fourteen GitHub-hosted `windows-latest` runners into temporary Windows test machines, so
several lanes can run Windows checks at once instead of queueing on victus.

```sh
tools/device-smoke/winpool.sh start 2 3     # 2 machines for 3 hours (1-5); prints winpool-1, winpool-2 when SSH answers
scp .build/$V/pb-windows-amd64.exe winpool-1:pb-new.exe
export SMOKE_WINDOWS_SSH=winpool-1; unset VICTUS_PASS
python3 tools/device-smoke/windows_lifecycle.py --expect-version $V --windows-install-json /tmp/winpool-1.json \
  --machine winpool-<lane>-1 --build 'C:\Users\runneradmin\pb-new.exe' install-pair doctor status
python3 tools/device-smoke/windows_checks.py --install-json /tmp/winpool-1.json --machine winpool-<lane>-1 --peer hp --only exec-main
tools/device-smoke/winpool.sh stop 1        # or: stop (all of yours); then remove the machine records:
tools/device-smoke/cleanup_test_account.py --prefix winpool- --stale '5 minutes'   # dry run, then --apply
```

- Machine N is the SSH host `winpool-N` (user `runneradmin`, an administrator; key `~/.ssh/winpool_controller`).
  `winpool.sh` writes `~/.ssh/winpool.conf` and includes it from `~/.ssh/config`.
- Name the Paperboat machine `winpool-…` so the cleanup finds it. `install-pair` mints its token on hp; nothing
  of Paperboat is stored in GitHub.
- Start and stop only from this controller (oracle): the start lock and the record of who started which machine
  are local to it. `stop` acts only on machines started from your worktree; never stop another lane's.
- A machine is gone after its hours, at `stop`, or when its run is cancelled; nothing on it survives.
- Evidence for a row ticked from a runner says so (`winpool, Windows Server 2025`); the final pass is on victus.

Send these to victus, they cannot run on a runner:

- anything that reboots, sleeps or logs the user off (the runner and its tunnel do not come back);
- anything that needs a signed-in desktop: tray or toast UI, a browser opened by `pb login`, per-user startup at logon;
- Windows Sandbox, Hyper-V guests, and rows that are about Windows 11 itself (the runner is Windows Server);
- LAN rows: direct peer-to-peer on one network, mDNS/local names, cutting the network (it cuts the tunnel);
- non-administrator installs and UAC prompts (the SSH session is already elevated, there is no second user);
- anything longer than 5 hours.

How it is wired: the runner starts sshd on loopback with the controller's public key (repository variable
`WINPOOL_CONTROLLER_PUBKEY`) and holds `ssh -N -R 127.0.0.1:2220N:127.0.0.1:22` to the jump host as user `winpool`
(secrets `WINPOOL_TUNNEL_KEY`, `WINPOOL_TUNNEL_HOST`, `WINPOOL_TUNNEL_KNOWN_HOSTS`). On the jump host that user has
no shell and `/etc/ssh/sshd_config.d/70-winpool.conf` allows it only those fourteen loopback listeners; the key's own `permitlisten=` options in `~winpool/.ssh/authorized_keys` must list the same fourteen ports (a port missing from either place makes `ssh -R` fail forever, 2026-11-10 slots 7-10). The controller
reaches a machine with `ProxyJump` through its own login on the jump host (`WINPOOL_JUMP`, default `ubuntu@anvit`).

## SSO from the Windows CLI (F19-03, F19-04)

`sso_windows.py` runs `pb login` (SSO sign-in, exit code read from the process) and the Require-SSO flow (a real non-SSO
member's Windows `pb team get` refused with `team_sso_required`, works when exempt and after Require SSO is off) on one
runner. Each result line has the device, the command and the exit code. Console sessions come from `console_session.mjs`
(`password EMAIL PASSWORD_FILE OUT` for a password account, `sso USER FIXTURE OUT` with `IDP_START` for an IdP user).

```sh
export SMOKE_WINDOWS_SSH=winpool-N PB_SMOKE_WINDOWS_PB='C:\Program Files\Paperboat\users\<id>\bin\pb.exe'   # pb install ran, no pair
python3 tools/device-smoke/sso_windows.py login --team T --sso-state alice.json --out f19-03.out
python3 tools/device-smoke/sso_windows.py require-sso --team T --member-state m.json --owner-state o.json --out f19-04.out
```
Each run enrolls the runner as a machine of the signed-in account: revoke that record by exact id afterwards.
