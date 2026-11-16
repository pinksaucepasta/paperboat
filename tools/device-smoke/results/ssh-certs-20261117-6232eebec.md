# Managed SSH on dev-6232eebec (2026-11-17, server .39): full L/M/W pass

dev-6232eebec on lanex2-l2, lanex2-m and lanex2-w (contains 164db337f per-user loopback source, 5177c7857 old CA line
rewritten, 6302166fc outdated certificates not reused, 70dd9ddd4 guard Revision 2, 110e211fd doctor check); the guard
was replaced on all three at 23:55 IST. XPLAT_ALLOW_FLEET_MAC=1, shared fleet-mac.lock + pbmac.lock.

## Mac cells, source checks and failed logins (started 18:34:19 UTC)

```
== versions
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-6232eebec
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-6232eebec
== Mac host CA line
cert-authority,principals="pb:machine_7cd4726b-92f2-4884-8b5a-ae400237
== L2 host CA line
cert-authority,principals="pb:machine_8139eee2-c661-484a-b1de-43be3f25
== L2 -> M
src=905d5da8a9129bd2
pb ssh: admin
plain ssh: admin
 Valid: from 2026-11-16T17:44:09 to 2026-11-17T05:45:09
 admin
 pb:machine_7cd4726b-92f2-4884-8b5a-ae40023764e2
scp=905d5da8a9129bd2
sftp=905d5da8a9129bd2
rsync=905d5da8a9129bd2
== M -> W (lanex2-w, Windows target)
src=5e914442facffce7
pb ssh: victus\pujan
plain ssh: victus\pujan
 Valid: from 2026-11-16T17:41:40 to 2026-11-17T05:42:40
 pb:machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1
scp=5e914442facffce7
sftp=5e914442facffce7
== M -> L2
src=f243298053fde6fc
pb ssh: tester
plain ssh: tester
scp=f243298053fde6fc
sftp=f243298053fde6fc
rsync=f243298053fde6fc
======
== M: lo0 source aliases, pb doctor
aliases=64
[('machine_guard_version', 'pass'), ('managed_ssh_sources', 'pass')]
== L: sshd sees M->L2 from the account's own source
tester
Accepted publickey for tester from 127.0.6.200 ID paperboat:user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2:session_147fc4ce-35a6-444d-b0a0-52b4870c4169:machine_8139eee2-c661-484a-b1de-43be3f25e126
Accepted publickey for tester from 127.0.6.200 ID paperboat:user_f0322398-3089-4ab3-8b5a-bbf5c1d728d2:session_147fc4ce-35a6-444d-b0a0-52b4870c4169:machine_8139eee2-c661-484a-b1de-43be3f25e126
== M (OpenSSH 10.3): account source from L2->M
admin
======
== M (OpenSSH 10.3): user A = 127.0.7.200 fails 6 times with a key sshd refuses
A try 1: admin@127.0.0.1: Permission denied (publickey,password,keyboard-interactive).
A try 2: admin@127.0.0.1: Permission denied (publickey,password,keyboard-interactive).
A try 3: admin@127.0.0.1: Permission denied (publickey,password,keyboard-interactive).
A try 4: admin@127.0.0.1: Permission denied (publickey,password,keyboard-interactive).
A try 5: admin@127.0.0.1: Permission denied (publickey,password,keyboard-interactive).
A try 6: admin@127.0.0.1: Permission denied (publickey,password,keyboard-interactive).
== B (the fleet account, source 127.0.6.200 via the bridge) right after
B pb ssh lanex2-m: admin
B pb ssh lanex2-m: admin
B plain ssh: admin
== M sshd log (last 4 min): penalties and B's source
```

The Mac's sshd (launchd, one sshd per connection) never drops a source, so A's refusals show only that B still logs in;
the penalty separation is proven on a 9.8+ sshd in CI (TestRealSSHDHonoursTheManagedCALines, run 43918325360, -race
-count=3).

## L2 <-> W

```
versions: ['dev-6232eebec'] ['dev-6232eebec']
W host CA line: cert-authority,principals="pb:machine_f5068171-773a-4d4b-a57c-6afe16712cd6",from
== L2 -> W  src 2fd224d86f2ad9e9
pb ssh: victus\pujan
plain ssh: victus\pujan
cert: Valid: from 2026-11-16T18:08:49 to 2026-11-17T06:09:49  pb:machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1
scp: 2fd224d86f2ad9e9
sftp: 2fd224d86f2ad9e9
== W -> L2  src 612cc969746685aa
pb ssh: tester
plain ssh (stdout to a file, Win32-OpenSSH #1748 inside sshd sessions): tester
scp: 612cc969746685aa
sftp: 612cc969746685aa
```

## W -> W (victus to itself, 2026-11-16 23:3x IST)

pb ssh and plain ssh PASS (victus\pujan); pb scp up/down 1 MiB sha256 equal on the rerun (the first run's mismatch did
not repeat; its scp output was not captured); certificate source-address 127.0.0.0/8,::1/128.

## scp round-trip hash loop (dev-6232eebec): the earlier single W>W mismatch did not recur

Each round: a fresh 1 MiB random file, pb scp up, sha256 + size on W, pb scp down, sha256 + size of the copy; a
mismatch keeps the files. The first W>W script hashed the downloaded copy without checking that it existed, so its one
"False" can equally be a missing download; its scp output was not captured.

W>W, one PowerShell script on victus (20 rounds):

```
W>W  1 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  2 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  3 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  4 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  5 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  6 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  7 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  8 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W  9 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 10 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 11 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 12 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 13 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 14 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 15 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 16 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 17 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 18 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 19 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W 20 up rc=0 remote=True/1048576 down rc=0 back=True/1048576 OK up:'' down:''
W>W mismatches: 0
```

L2>W, one bash loop on lanex2-l2 (5 rounds; hash on W over pb ssh):

```
L>W 1 up rc=0 remote_ok=True/1048576 down rc=0 back_ok=True/1048576 OK up:'' down:''
L>W 2 up rc=0 remote_ok=True/1048576 down rc=0 back_ok=True/1048576 OK up:'' down:''
L>W 3 up rc=0 remote_ok=True/1048576 down rc=0 back_ok=True/1048576 OK up:'' down:''
L>W 4 up rc=0 remote_ok=True/1048576 down rc=0 back_ok=True/1048576 OK up:'' down:''
L>W 5 up rc=0 remote_ok=True/1048576 down rc=0 back_ok=True/1048576 OK up:'' down:''
L>W mismatches: 0
```

Earlier harness loop (one call per step): W>W rounds 1-5 OK at every step before its time limit.

## Penalty separation on a real OpenSSH 10.0 sshd (pbvm2 throwaway containers, dev-6232eebec)

Two Debian trixie containers on pbvm2 (OpenSSH_10.0p2), enrolled as throwaway machines (the operator account's pb on
pbvm2 minted the tokens): sshc-pen (target) and sshc-penc (client). The client's managed login reaches the target's
sshd from the account's own source:

```
A try 1: pbt@127.0.0.1: Permission denied (publickey,password).
A try 2: pbt@127.0.0.1: Permission denied (publickey,password).
A try 3: pbt@127.0.0.1: Permission denied (publickey,password).
A try 4: pbt@127.0.0.1: Permission denied (publickey,password).
A try 5: Connection reset by 127.0.0.1 port 22
A try 6: Connection reset by 127.0.0.1 port 22
A try 7: Connection reset by 127.0.0.1 port 22
A try 8: Connection reset by 127.0.0.1 port 22
B pb ssh sshc-pen: pbt
B pb ssh sshc-pen: pbt
A again after B: Connection reset by 127.0.0.1 port 22
== sshd log
Nov 16 19:17:24 sshc-pen sshd[87]: drop connection #0 from [127.0.3.200]:39869 on [127.0.0.1]:22 penalty: failed authentication
Nov 16 19:17:24 sshc-pen sshd[87]: drop connection #0 from [127.0.3.200]:49033 on [127.0.0.1]:22 penalty: failed authentication
Nov 16 19:17:24 sshc-pen sshd[87]: drop connection #0 from [127.0.3.200]:42941 on [127.0.0.1]:22 penalty: failed authentication
Nov 16 19:17:24 sshc-pen sshd[87]: drop connection #0 from [127.0.3.200]:56885 on [127.0.0.1]:22 penalty: failed authentication
Nov 16 19:17:25 sshc-pen sshd-session[2123]: Accepted publickey for pbt from 127.0.2.200 port 39125 ssh2: ED25519-CERT SHA256:+bXKQN4DcEn5gVUTS1KoRI6hSKzWwdgQLFUX5tadBNY ID paperboat:user_…
Nov 16 19:17:25 sshc-pen sshd-session[2133]: Accepted publickey for pbt from 127.0.2.200 port 34057 ssh2: ED25519-CERT SHA256:+bXKQN4DcEn5gVUTS1KoRI6hSKzWwdgQLFUX5tadBNY ID paperboat:user_…
Nov 16 19:17:27 sshc-pen sshd-session[2143]: Accepted publickey for pbt from 127.0.2.200 port 34697 ssh2: ED25519-CERT SHA256:+bXKQN4DcEn5gVUTS1KoRI6hSKzWwdgQLFUX5tadBNY ID paperboat:user_…
Nov 16 19:17:28 sshc-pen sshd[87]: drop connection #0 from [127.0.3.200]:45671 on [127.0.0.1]:22 penalty: failed authentication
```

User A (127.0.3.200) was refused 4 times, then sshd dropped it (PerSourcePenalties); user B (the client's pb ssh from
127.0.2.200) logged in twice right after, and A stayed dropped. Both machine records were revoked by exact id
(machine_c191450f..., machine_619d47a3...) and the containers and image removed.
