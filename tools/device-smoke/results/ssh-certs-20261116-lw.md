# SSH certificates device check, Linux <-> Windows (2026-11-16, server .29)

Machines: L = sshc-l (hp container, Ubuntu, user tester), W = sshc-w-2 (winpool-7, Windows Server 2025 runner,
user runneradmin). Builds: dev-107aee32e (06:20-06:52 UTC), then both upgraded in place to dev-c7dd02a3c (main c7dd02a3c).

| Part | Result | Build |
|---|---|---|
| L->W pb ssh (whoami, hostname) | PASS | 107aee32e, c7dd02a3c |
| L->W plain ssh runneradmin@sshc-w-2.ppr.boats | PASS | both |
| L->W pb scp up/down 1 MiB, sha256 equal | PASS | both |
| L->W pb sftp put/get, sha256 equal | PASS | both |
| L->W pb rsync | exempt (no rsync on Windows, as F5-03 W) | |
| W->L pb ssh | PASS | both |
| W->L plain ssh tester@sshc-l.ppr.boats | FAIL on 107aee32e (Match exec //?/ path: Match not applied, client hung); PASS on c7dd02a3c | fix c7dd02a3c |
| W->L pb scp up/down, sha256 equal | FAIL on 107aee32e (same cause); PASS on c7dd02a3c | |
| W->L pb sftp put/get, sha256 equal | PASS on c7dd02a3c | |
| Certificate: principals runneradmin + pb:<machine>:<token>, source-address loopback, 12 h validity, key id user/session/machine | PASS | |
| Host CA line (L ~/.ssh/authorized_keys, W ProgramData ...\ssh\authorized_keys\paperboat): one cert-authority line, principals=, from= loopback | PASS | |
| Renewal: connection after 30 s gets a new cert (valid-from 06:47:52), second within 30 s reuses it (same serial) | PASS | 107aee32e |
| Revocation: pb logout on L -> W host CA line principal :0 -> :a228efa85d25f8ef within 12 s; the held cert (principal :0) no longer matches; after pb login the new session's cert carries the new principal and logs in | PASS | 107aee32e |
| Env: pb ssh sshc-w-2 -- set and -- cmd.exe /c set: 102 vars incl. USERNAME, PATH, USERPROFILE, SystemRoot | PASS | both |
| Env: -- cmd /c set | upstream Win32-OpenSSH bug #1373 (sshd wraps as cmd.exe /c "cmd /c set" -> 'set"'); plain ssh behaves the same | |

Not done: macOS (no macpool slot; fleet lanex2-m runs a pre-certificate build).
Timing (hp container -> winpool via relay/india): plain ssh exit 2.6-3.5 s with a reused cert, 5.5 s when renewing.
