# X-36 version skew by hand (R-X-36B), 2026-11-13 IST 21:00-23:30, first part

Newest build under test: dev-982827507 (main 982827507; main later moved to 2830fb517, not used). Previous build: dev-918afefe9.
Own throwaway trio on the prod server, one fresh console account: `x36b-l` (Linux container on pbvm3, later pbvm2 after pbvm3 stopped
answering ssh; fresh install, fresh pair), `x36b-w` (winpool-2, Windows Server runner), `x36b-m` (macpool-4, macOS arm64 hosted runner),
controller container `laner-x36-ctl` (signed in with a one-day API token of the account). `pb version` of every machine at the start:
x36b-l, x36b-w, x36b-m all `dev-982827507`. Each skew run below does a fresh install (uninstall, install, pair) of the previous build on
the "old" machine, runs the cross-OS checks of both directions against the other machines (new build), then reinstalls the new build.
Harness: tools/device-smoke/xplat_skew.py (xplat_term/exec/ssh/send/access/tunnel/config); the Windows config fixture needed
XPLAT_REPO_ADDR/XPLAT_REPO_PORT (pbvm3 is not reachable from the controller by port; an `ssh -L` made it reachable), a 4-line harness change.

## L old, W new (dev-918afefe9 on x36b-l)
```
PASS X-36:ssh:L:W 191s 4 checks on L(dev-918afefe9)<->W(dev-982827507): 4 worked (xplat-ssh-20261113T152629Z.json)
PASS X-36:config:L:W 1074s 4 checks on L(dev-918afefe9)<->W(dev-982827507): 4 worked (xplat-config-20261113T160627Z.json)
(first config run failed on the harness: the repository host was not reachable from the Windows runner; rerun with the tunnel passed)
```
## W old, L new (dev-918afefe9 on x36b-w)
```
PASS X-36:config:W:L 552s 4 checks on W(dev-918afefe9)<->L(dev-982827507): 4 worked (xplat-config-20261113T154307Z.json)
FAIL X-36:ssh:W:L (1st run): L>W `pb ssh x36b-w -- hostname` printed "could not reach x36b-w: it may be offline, or its Paperboat service is not running" (first check after the fresh install; failures.md)
PASS X-36:ssh:W:L (rerun, same pair) 182s 4 checks: 4 worked (xplat-ssh-20261113T155833Z.json)
```
Same shape as the predecessor's one W>L failure: a first ssh right after the fresh install of the old build, gone on rerun.

## M old, L and W new (dev-918afefe9 on x36b-m, macpool-4)
```
PASS X-36:terminal:M:L (xplat-term-20261113T164814Z.json)   PASS X-36:exec:M:L (xplat-exec-20261113T164929Z.json)
PASS X-36:ssh:M:L (xplat-ssh-20261113T164951Z.json)         PASS X-36:send:M:L (xplat-send-20261113T165857Z.json)
PASS X-36:access:M:L (xplat-access-20261113T170031Z.json)   PASS X-36:config:M:L (xplat-config-20261113T170259Z.json)
FAIL X-36:tunnel:M:L: first request to the M-origin tunnel URL answered 504 origin_timeout (X-Paperboat-Error header)
PASS X-36:terminal:M:W (xplat-term-20261113T171138Z.json)   PASS X-36:exec:M:W (xplat-exec-20261113T171245Z.json)
PASS X-36:ssh:M:W (xplat-ssh-20261113T171311Z.json)         PASS X-36:send:M:W (xplat-send-20261113T172250Z.json)
PASS X-36:access:M:W (xplat-access-20261113T172515Z.json)
FAIL X-36:tunnel:M:W: same 504 origin_timeout on the first request
FAIL X-36:config:M:W: `pb config sync validate` on x36b-m timed out (120 s) in the harness
```
By hand on M (old build) afterwards: a new tunnel to a local origin answered 504 origin_timeout on the first request, then 200 on 3 HTTP/1.1 and
1 HTTP/2 requests; `pb tunnel status` said ready with edge_connected. Probe tunnel deleted. Cause not isolated yet (cold start of a hosted
Mac origin versus skew); the next runs have M on the new build.

## Open (named gaps)
- M<->L and M<->W tunnel, M<->W config (above), rerun pending; L<->M and W<->M with M new and the other old: running.
- terminal, exec, send, access, tunnel for L<->W on dev-982827507: only the predecessor's pass on dev-fb95e62dd (xplat-manual-X-36-20261113T1100Z-LW.md).
- Update of one machine mid-session keeping the other machines' sessions (incl. the inbound-ssh refusal of 5bd365864): not run on this build.
- No cell ticked.
