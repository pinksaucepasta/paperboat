# SSH certificates: revoke user A, user B keeps working (2026-11-16, server .31 = 03c7d234f)

Target sshc-l (hp container, Linux, dev-03c7d234f). A = sshc-b, B = sshc-b2 (pbvm2 containers, Linux, dev-03c7d234f),
same account, separate CLI sessions. Script: pbvm2 ~/laneh/sshc-revoke.sh.

| Step | Result |
|---|---|
| Target CA line after .31: `cert-authority,principals="pb:<machine>",from=loopback` (stable, no token) | PASS |
| A and B hold certificates for sshc-l (A serial 14781218127322098334, B serial 16879727918525816989; key IDs paperboat:<user>:<session>:<machine>) | PASS |
| A and B each hold an open `pb ssh sshc-l` session printing a tick per second | PASS |
| `pb logout` on A: A's open session ends 1.96 s later with "access to sshc-l was revoked: this sign-in was ended" | PASS |
| A's next `pb ssh` refused (sign-in ended); A's plain `ssh` with its still-valid certificate refused | PASS |
| B's open session unaffected: 60/60 ticks, exit 0 | PASS |
| B's next `pb ssh sshc-l -- whoami` works with the same certificate (serial unchanged: no renewal, no control-plane request) | PASS |
