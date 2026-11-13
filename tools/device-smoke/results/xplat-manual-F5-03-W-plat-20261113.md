# F5-03 Windows — 2026-11-13, cycle 1

Machines: winpool-1 (Paperboat plat-w, Windows client) and plat-s3 (Linux target,
container on hp). Both `pb version`: **dev-aee62911e**. No binaries changed.
Versions verified with `ssh winpool-1 "pb version"` and
`ssh hp "docker exec -u tester plat-s3 bash -lc 'pb version'"`.
MSYS2 rsync 3.5.1; target rsync 3.2.7. Inbox OpenSSH 9.5p2;
machine-installed native OpenSSH 10.0p2.

## Earlier cycle-1 checks supplied by the parent

Existing harness command (Windows cmd):
`set PATH=%PATH%;C:\msys64\usr\bin&& python resume_check.py --pb pb --target plat-s3 --tools scp sftp --kill-at 12`
SCP resume was rerun separately with `--tools scp --only-resume --kill-at 12`.
Source logs: `/home/ubuntu/plat-bin/out/f503-w.txt` (SCP large tree) and
`/home/ubuntu/plat-bin/out/f503-w3.txt` (nonzero SCP partials, SFTP).

```text
PASS scp up large-tree ok 602 files 613s
PASS scp down large-tree ok 602 files 967s
PASS scp up resume partial=51249152/209715200 resumed ok 32s hash match=True
PASS scp down resume partial=45121536/209715200 resumed ok 42s hash match=True
PASS sftp up large-tree ok 602 files 479s
PASS sftp down large-tree ok 602 files 992s
PASS sftp up resume partial=58880000/209715200 resumed ok 24s hash match=True
PASS sftp down resume partial=48829440/209715200 resumed ok 31s hash match=True
```

## Default rsync failure and diagnosis

Windows command: `set PATH=%PATH%;C:\msys64\usr\bin&& pb rsync -avv C:\Users\runneradmin\r10.bin plat-s3:/tmp/rx/r10.bin`.
Run over plain `ssh winpool-1`, piped input `(echo.) | pb rsync ...`, and
`ssh -tt winpool-1` with a live local PTY. All three fail identically:

```text
/usr/bin/cat: -: Invalid argument
rsync: connection unexpectedly closed (0 bytes received so far)
rsync error: error in rsync protocol data stream (code 12)
```

Not a console-stdin-only bug. With the original appended PATH, MSYS2 bash
`command -v cat` selects `/c/Program Files/Git/usr/bin/cat`, crossing runtime
boundaries. Explicit `/usr/bin/cat` removes Invalid argument but native-SSH
pipe bridging still stalls. Bounded probes confirm the stall with both inbox 9.5 and machine-installed
10.0 when the cat wrapper is used (10.0 reports rsync timeout after 20 seconds).
`pb ssh plat-s3 -- echo f503-marker` succeeds.

Wrapper history: introduced in `130bfafe2c5c79f42403431dc400cbb38521ad26`;
no differences through `aee62911e` on ssh_tool_execute_windows.go or
ssh_tool_posixpaths.go. Its earlier successful console check covered 154 bytes,
not a large binary transfer. Input process substitution avoids waiting for
rsync to close stdin before the shell exits; output copying drains SSH output,
and pipefail propagates SSH errors. Reference read:
`/home/ubuntu/ref/tailscale/ssh/tailssh/tailssh.go:1047` also excludes the stdin
copier from the output wait group to avoid client-half-close deadlock.

## Explicit remote shell on cycle 1

`ssh winpool-1 "set PATH=C:\msys64\usr\bin;%PATH%&& set RSYNC_RSH=ssh&& python resume_check.py --pb pb --target plat-s3 --tools rsync --kill-at 12"`

This chooses `C:\Program Files\OpenSSH\ssh.exe` (10.0) directly, without the
cat wrapper. A separate 10 MiB `pb rsync --timeout=20 -av --rsh=ssh ...` passed
(exit 0). Full-run output and manual resume results follow below.

Direct inbox OpenSSH 9.5 also passes:
`set PATH=C:\msys64\usr\bin;%PATH%&& pb rsync --timeout=20 -avv --rsh=/c/Windows/System32/OpenSSH/ssh.exe C:\Users\runneradmin\r10.bin plat-s3:/tmp/rx/r10-direct95.bin`
(exit 0, 10,485,760 bytes). Thus the bridge itself is unnecessary for the
current MSYS2 installation, and is removed in the final fix. The earlier
successful reference build covered a 154-byte transfer; it does not establish
that large binary transfers through the wrapper ever worked.

Manual resume commands (Windows, same PATH and `RSYNC_RSH=ssh`):

```text
pb rsync -a --partial --inplace --append-verify --timeout=60 --bwlimit=2048 C:/Users/runneradmin/AppData/Local/Temp/f503-wimpl-ms3cb4l1/source.bin plat-s3:/tmp/f503-wimpl-20261113/source.bin
# interrupt after 12 seconds: taskkill /F /T /PID <this pb PID>
pb ssh plat-s3 -- stat -c %s /tmp/f503-wimpl-20261113/source.bin
pb rsync -a --partial --inplace --append-verify --timeout=60 C:/Users/runneradmin/AppData/Local/Temp/f503-wimpl-ms3cb4l1/source.bin plat-s3:/tmp/f503-wimpl-20261113/source.bin
pb ssh plat-s3 -- sha256sum /tmp/f503-wimpl-20261113/source.bin
pb rsync -a --partial --inplace --append-verify --timeout=60 --bwlimit=2048 plat-s3:/tmp/f503-wimpl-20261113/source.bin C:/Users/runneradmin/AppData/Local/Temp/f503-wimpl-ms3cb4l1/down.bin
# interrupt after 12 seconds: taskkill /F /T /PID <this pb PID>
pb rsync -a --partial --inplace --append-verify --timeout=60 plat-s3:/tmp/f503-wimpl-20261113/source.bin C:/Users/runneradmin/AppData/Local/Temp/f503-wimpl-ms3cb4l1/down.bin
```

Source: generated random 200 MiB file; compare local SHA-256 to remote and
resumed download. No file contents recorded. `--inplace` makes the partial file
available for reuse after Windows process-tree termination. The existing
harness's initial rsync command omits `--partial` and `--inplace`; its resume
verdict is kept separately and cannot substitute for this explicit check.

Observed cycle-1 results (existing harness for large trees, explicit manual
commands above for resume):

```text
PASS rsync up large-tree ok 602 files 58s
PASS rsync down large-tree ok 602 files 403s
PASS rsync up resume partial=18776064/209715200 resumed=True hash_match=True
PASS rsync down resume partial=5767168/209715200 resumed=True hash_match=True
```

All twelve named SCP/SFTP/rsync transfer parts have passing cycle-1 evidence;
rsync requires the explicit `RSYNC_RSH=ssh` selection documented above.
The default rsync shell path on cycle 1 remains broken. CHECKLIST cells were
not edited. Compiled CLI fix verification belongs to the next cycle.

Regression validation: the old generated shell returns 42 and loses binary
bytes when an incompatible cat precedes the runtime on PATH. The final
`TestRsyncRemoteShellPreservesBinaryStream` tests binary bytes and exact exit
status through the actual generated remote-shell argument with a hostile
helper PATH. The earlier string-only coverage missed this behavior.
