# X-28 leftovers by hand (my own throwaway machines), 20261112T192538Z (UTC)

pb dev-6c7e29534 on all (a throwaway Linux container on anvit, a hosted Windows runner), prod server.

## trust-host after a target reset (client Linux -> target Windows, then Windows -> Linux)
```
Linux client -> Windows target, target reset (`pb reset` with a token) and re-paired under the SAME name:
  before: pb ssh xpc-w -- hostname -> the host name
  after the reset and re-pair (six tries over ~2 min): the same host name every time, no identity-change message, no trust-host needed
Windows target, sshd host keys deleted and regenerated (Stop-Service sshd; Remove-Item ssh_host_*; Start-Service sshd, 3 new keys):
  pb ssh doctor host_identity before and after: SHA256:8+EGa4... (unchanged); pb ssh keeps working (8 tries over 2 min), no prompt
Windows client -> Linux target: "SSH to xpc-l is not ready: Managed SSH is not ready" before and after the reset, and `pb ssh doctor xpc-l` answers not_found on that client: its machine inventory did not contain the re-paired Linux machine (see failures.md)
```
Reading: Paperboat's host identity is the one the server publishes for the machine id, not the OS sshd key, so re-pairing under the same name and regenerating the OS host keys do not produce a pending identity change; there was no way here to put `pb ssh trust-host` into play, so that part of the row stays unseen.

## A user name with a space or a non-ASCII letter (Windows target is the same machine as above)
OS user "ZoeTest" (plain) installs pb and pairs as a second user; OS users "Zoe Test" (space) and "Zoë Test" fail `pb pair` with the server answer "invalid machine bootstrap (HTTP 500)" (failures.md). Linux names with spaces are refused by pb's linger check by design (`^[a-z_][a-z0-9_.-]{0,31}$`), a Linux row part that does not apply.
