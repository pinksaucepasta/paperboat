# Device smoke

## Extra Windows machines (winpool)

`winpool.sh` turns up to six GitHub-hosted `windows-latest` runners into temporary Windows test machines, so
several lanes can run Windows checks at once instead of queueing on victus.

```sh
tools/device-smoke/winpool.sh start 2 3     # 2 machines for 3 hours (1-5); prints winpool-1, winpool-2 when SSH answers
scp .build/$V/pb-windows-amd64.exe winpool-1:pb-new.exe
export SMOKE_WINDOWS_SSH=winpool-1; unset VICTUS_PASS
python3 tools/device-smoke/windows_lifecycle.py --expect-version $V --windows-install-json /tmp/winpool-1.json \
  --machine winpool-<lane>-1 --build 'C:\Users\runneradmin\pb-new.exe' install-pair doctor status
python3 tools/device-smoke/windows_checks.py --install-json /tmp/winpool-1.json --machine winpool-<lane>-1 --peer hp --only exec-main
tools/device-smoke/winpool.sh stop 1        # or: stop (all); then remove the machine records:
tools/device-smoke/cleanup_test_account.py --prefix winpool- --stale '5 minutes'   # dry run, then --apply
```

- Machine N is the SSH host `winpool-N` (user `runneradmin`, an administrator; key `~/.ssh/winpool_controller`).
  `winpool.sh` writes `~/.ssh/winpool.conf` and includes it from `~/.ssh/config`.
- Name the Paperboat machine `winpool-…` so the cleanup finds it. `install-pair` mints its token on hp; nothing
  of Paperboat is stored in GitHub.
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
no shell and `/etc/ssh/sshd_config.d/70-winpool.conf` allows it only those six loopback listeners. The controller
reaches a machine with `ProxyJump` through its own login on the jump host (`WINPOOL_JUMP`, default `ubuntu@anvit`).
