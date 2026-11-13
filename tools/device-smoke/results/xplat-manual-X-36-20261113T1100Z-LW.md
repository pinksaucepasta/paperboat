# X-36 version skew, L<->W, by hand on today's main (2026-11-13, IST evening)

Own throwaway trio, one fresh account: `x36-l` (Linux container on pbvm3), `x36-w` (winpool-8, Windows Server runner), controller container `x36-ctl`. New build = `dev-fb95e62dd` (main fb95e62dd), previous build = `dev-918afefe9`. `pb version` on every machine recorded below. No Mac: the macpool queue gave no runner (all 8 slots claimed; two `macpool.sh start` attempts timed out in GitHub's queue).

## Part: a .pb-update bundle made on one OS is refused on the other (both on dev-fb95e62dd)
```
x36-l dev-fb95e62dd, x36-w dev-fb95e62dd: `pb update bundle` made on each; copied with scp; `pb update --from <bundle>` on the other
PASS L>W (bundle made on Linux, applied on Windows): update_source_unreadable: this .pb-update bundle was made on linux and this machine is windows; run `pb update bundle` on a windows machine. Nothing was changed. Version dev-fb95e62dd before and after.
PASS W>L (bundle made on Windows, applied on Linux): update_source_unreadable: this .pb-update bundle was made on windows and this machine is linux; run `pb update bundle` on a linux machine. Nothing was changed. Version dev-fb95e62dd before and after.
`pb exec x36-w -- hostname` from L and `pb exec x36-l -- hostname` from W worked after the refusals.
```

## Part: previous build on L, new build on W (xplat_skew.py skew --old L; fresh install of dev-918afefe9 on L, new on W)
```
PASS X-36:terminal:L:W 52s 4 checks on L(dev-918afefe9)<->W(dev-fb95e62dd): 4 worked, 0 refused (xplat-term-20261113T111727Z.json)
PASS X-36:exec:L:W 77s 4 checks: 4 worked, 0 refused (xplat-exec-20261113T111845Z.json)
PASS X-36:send:L:W 409s 2 checks: 2 worked (xplat-send-20261113T112816Z.json)
PASS X-36:access:L:W 102s 2 checks: 2 worked (xplat-access-20261113T112959Z.json)
PASS X-36:tunnel:L:W 231s 2 checks: 2 worked (xplat-tunnel-20261113T113350Z.json)
FAIL X-36:ssh:L:W: X-18:ssh:W>L `pb ssh x36-l -- hostname` returned the generic "Paperboat could not finish the command" (xplat-ssh-20261113T111909Z.json); L>W ssh and both scp directions passed in the same run. Not a message naming the older machine.
FAIL X-36:config:L:W: xplat_config.py could not run (repository host unreachable from the container at the harness default; not a skew result)
```

## Part: previous build on W, new build on L (skew --old W; fresh install of dev-918afefe9 on W)
```
PASS X-36:terminal:W:L 71s 4 checks on W(dev-918afefe9)<->L(dev-fb95e62dd): 4 worked, 0 refused (xplat-term-20261113T114300Z.json)
PASS X-36:exec:W:L 79s 4 checks: 4 worked (xplat-exec-20261113T114419Z.json)
PASS X-36:send:W:L 421s 2 checks: 2 worked (xplat-send-20261113T115120Z.json)
PASS X-36:access:W:L 95s 2 checks: 2 worked (xplat-access-20261113T115255Z.json)
PASS X-36:tunnel:W:L 213s 2 checks: 2 worked (xplat-tunnel-20261113T115629Z.json)
ssh and config were skipped in this run (--features), then not completed: see below.
```

## Not completed (named gaps)
- ssh and config sync, both skew directions: reruns failed on infrastructure: pbvm2, pbvm3 and pbvm4 timed out on ssh banner exchange and hp swapped (load 144-250, swap full) so enrollment tokens, `docker exec` writes and uninstalls timed out; the old build's `pb uninstall` on L once failed at stage `uninstall_lock` on the loaded container (a retry succeeded). The only product-looking result is W>L `pb ssh` failing with the generic message; a later `pb ssh` and `pb exec` from W to L also failed ("could not reach the target machine") while L was online in `pb status`, so it is not isolated from the host overload. Not reproduced on a quiet host.
- Mid-session update of the target (inbound `pb ssh` into the target now counts as open since 5bd365864, so the update must refuse): not run.
- Preview; official build below the minimum (staging); every Mac pair (M old, M new, bundles with M, replace running binary on M, mid-session on M): not run, no Mac runner.
- Nothing ticked.
