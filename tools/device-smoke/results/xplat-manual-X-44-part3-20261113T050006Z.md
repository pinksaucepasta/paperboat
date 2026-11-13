# X-44 part 3 by hand: Windows refusal page fixed, all parts re-run on one build, UTC 20261113 05:00 (IST 10:30)

Hosts and clients (all dev-bb9da7fb1 = main bb9da7fb1, prod server, throwaway accounts B and C):
Linux amd64 systemd container (pbvm2) with OS users pbt (account B, machine rx44-l) and pbt2 (account C, machine rx44-lc).
Windows runner winpool-2 with OS users runneradmin (account B, machine rx44-w-2) and zoec (account C, machine rx44-wc, a local administrator reached through a scheduled task with its password) and a third local user strangerx with no Paperboat install.
Before the fix (dev-051c1f7f2): `curl.exe -v http://127.100.0.1/` as strangerx and as SYSTEM -> "connect ... failed: Bad access" (rc 7), no refusal page. Cause: the WFP connect block on 127.100.0.0/16 had no permit for the gateway 127.100.0.1:80/443 (wfp/paperboat_windows.go). Fix bb9da7fb1 (permit connect to the gateway ports for any user; handoff attributes and refuses).

## Refusal page on the Windows gateway: PASS on dev-bb9da7fb1
```
strangerx (no daemon):  curl -v http://127.100.0.1/ -> HTTP/1.1 403 Forbidden, "This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user."
NT AUTHORITY\SYSTEM (scheduled task): same 403 page
runneradmin / zoec (own daemon): connect succeeds, proxied (421 Unknown or mismatched Paperboat browser host for a bare IP request)
```
== versions
linux pbt: Version dev-bb9da7fb1
linux pbt2: Version dev-bb9da7fb1
win runneradmin: Version dev-bb9da7fb1
== Linux host users, from Linux clients
pbt (B) exec rx44-w-2: runnervmm7ud1\runneradmin
pbt (B) exec rx44-wc : pb: No machine has that name or ID. Run `pb machine list` to
pbt2 (C) exec rx44-wc: runnervmm7ud1\zoec
pbt2 (C) exec rx44-w-2: pb: No machine has that name or ID. Run `pb machine list` to
== Windows client users
runneradmin (B) exec rx44-l : pbt
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_8921183f-0d30-46cf-aafb-479528d5b6f1. Run `pb bugreport --upload` to send the details.
runneradmin (B) exec rx44-lc: 
zoec (C):
  ƒ-,ƒ-,ƒ-?ƒ-?ƒ-?ƒ-?ƒ-?ƒ-?ƒ-~ƒ-,ƒ-,   Version dev-bb9da7fb1
pbt2
pb: No machine has that name or ID. Run `pb machine list` to see your machines. 
== machine lists
pbt: [('rx44-l', True, '127.100.0.2'), ('rx44-w', False, '127.100.0.3'), ('rx44-w-2', True, '127.100.0.4')]
pbt2: [('rx44-lc', True, '127.100.0.2'), ('rx44-wc', True, '127.100.0.3')]

== simultaneous: 4 loops at once x12 on dev-bb9da7fb1
win runneradmin(B)->rx44-l whoami:
12
win zoec(C)->rx44-lc whoami (pbt2):
12
linux host users resolving their other-OS machine:
     12 pbt rx44-w-2 runnervmm7ud1
     12 pbt2 rx44-wc runnervmm7ud1

## Not run: any macOS host or client (macpool queue: runs pending 30+ min on GitHub's macOS runners)
