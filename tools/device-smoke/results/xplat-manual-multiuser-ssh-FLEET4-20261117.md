# Managed SSH to two OS users of one account on one host, and a cloned host (FIX-MULTIUSER-SSH 29c7ab1c9; lane FLEET4, 2026-11-17 03:20-03:30 IST)

Build dev-479e6f8a9 (client pbvm2 operator pb and both hosts), server 2026.11.13.48 (migration 375 applied). One Linux container on pbvm2 (Ubuntu 24.04 systemd, openssh-server installed), OS users `pbt` and `pbt2`, each running its own Paperboat daemon, both paired to the same account as `fmu-u1-2` (pbt) and `fmu-u2` (pbt2); the clone is a second container with the first container's /etc/ssh/ssh_host_* copied in, paired as `fmu-clone-m-2`.

```
pb ssh fmu-u1-2 -- whoami   -> pbt     rc 0
pb ssh fmu-u2   -- whoami   -> pbt2    rc 0       (before this fix: refused as cloned, ft2-u2, failures.md L478)
pb exec fmu-u1-2 / fmu-u2   -> pbt / pbt2
host key fingerprints container vs clone: identical (SHA256:Z6Kh...Vg)
pb ssh fmu-clone-m-2 -- whoami -> refused: "The machine's SSH host keys are identical to fmu-u1-2's. This machine was cloned with its SSH host keys. On it, remove /etc/ssh/ssh_host_*, run ssh-keygen -A and restart sshd" rc 1
pb exec fmu-clone-m-2 -- whoami -> pbt (exec is not ssh, still works)
original users after the clone appeared: pb ssh fmu-u1-2 -> pbt, fmu-u2 -> pbt2 (rc 0)
```
Harness notes: the image has no sshd and `pbt2` has no sudo, so a first run on 21:36 UTC failed for those reasons (not product); records from that run were revoked by exact id. Cleanup done: containers removed, 5 records revoked.
