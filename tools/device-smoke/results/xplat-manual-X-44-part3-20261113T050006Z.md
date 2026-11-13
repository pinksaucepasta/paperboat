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


## Refusal page on the Linux gateway, dev-bb9da7fb1 (container, OS users root, stranger without a daemon, pbt with one): PASS
```
== root
HTTP/1.1 403 Forbidden
Content-Type: text/plain; charset=utf-8
Connection: close
Content-Length: 92

This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.
== stranger
HTTP/1.1 403 Forbidden
Content-Type: text/plain; charset=utf-8
Connection: close
Content-Length: 92

This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.
== pbt (own daemon)
HTTP/1.1 421 Misdirected Request
```

L<->W: every part passed on dev-bb9da7fb1 on both machines (Linux container, Windows runner winpool-2): each OS as host and as client, address per account, one account never reaches the other, refusal page on both gateways, simultaneous connect.

## Mac runner macpool-3 (macOS 26.7.2, user runner, account B machine rx44-m, dev-bb9da7fb1) as client and host of the Linux and Windows machines
```
== Mac runner (account B, machine rx44-m) Version dev-bb9da7fb1
mac exec rx44-l : pbt
mac exec rx44-w-2: runnervmm7ud1\runneradmin
mac exec rx44-lc (account C): pb: No machine has that name or ID. Run `pb machine list` to
linux pbt exec rx44-m: runner
linux pbt2 (C) exec rx44-m: pb: No machine has that name or ID. Run `pb machine list` to
win runneradmin exec rx44-m: pb: Paperboat could not reach the target machine. It may be offline, or its netw
(first Windows->Mac exec: "could not reach the target machine" right after the Mac paired; the next two tries returned `runner`)
```
## Mac as host with two OS users: FAIL (logged in failures.md, not producible on macpool)
A second OS user macc (uid 502, passwordless sudo, file-fallback config) installs, but `pb pair` as that user prints "Completing one-shot machine enrollment..." and is killed by SIGHUP (rc 129); doctor then says awaiting_enrollment and hostd not running. F23-01's M [x] used a real login session user.

## Test of the Windows fix on a runner without a guard (after `pb uninstall` of both users), device
```
== wfp
--- PASS: TestApplyLetsAnyUserConnectToTheGateway (0.28s)
PASS
== wfp-nofix
    paperboat_windows_test.go:198: a user without a lease is blocked from the gateway: no refusal page can reach it
--- FAIL: TestApplyLetsAnyUserConnectToTheGateway (0.27s)
FAIL
```
machineguard device tests on the fixed tree (Gateway|Route|Attribution|Refusal): all PASS (TestGatewayRoutesTwoOSUsersOnDevice skips without its fixture). CI: tools/ci-test.sh cli -run 'Gateway|WFP|...' at 26d61874b: success, 0 FAIL, machineguard ok (run 43369499902).
## Open: L<->M and M<->W (a Mac host with two OS users); everything else of those pairs passed.
