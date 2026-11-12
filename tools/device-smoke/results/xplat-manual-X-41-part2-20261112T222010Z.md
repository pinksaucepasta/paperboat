# X-41 part 2 by hand on my own account-C pair, UTC 20261112 22:05 (IST 03:35 13 Nov)

xpr-lc = Linux (user tester2, account C), xpq-wc = Windows (OS user ZoeC, account C), pb dev-sendfix (main + two send fixes). The Windows machine had a durable tunnel (xpc41-t) and a preview; the Linux user revoked it by exact machine id with `pb machine revoke <id> --confirm`.
## Linux observes a revoked Windows machine: PASS for tunnels, previews, names
```
before: machines xpq-wc online; tunnels xpc41-t; previews 1; ssh aliases xpq-wc, xpr-lc, xpr-wc
+5 s:   machines list has no xpq-wc, tunnels [], previews [], ssh alias for xpq-wc gone (xpr-lc, xpr-wc stay), no change at +30 s and +90 s
`pb exec xpq-wc -- whoami` afterwards: "No machine has that name or ID"
```
## Still open for X-41
Any pair with a Mac, Linux removed seen from Windows (the Windows client's managed ssh config, FIX-WU), ENV delivery, config-sync entries and shared sessions to the removed machine, the uninstall flavour.
