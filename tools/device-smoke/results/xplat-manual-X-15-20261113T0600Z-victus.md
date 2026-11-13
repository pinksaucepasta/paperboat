# X-15 Windows parts on the real Windows machine (fleet lanex2-w = Victus), 2026-11-13 10:15-11:10 IST

`pb version` on lanex2-l, lanex2-m, lanex2-w: dev-051c1f7f2 (server 2026.11.13.13).

## Scripted (tools/device-smoke/xplat_send.py --only batch,modes,refuse,inboxset)
L,W: xplat-send-20261113T044457Z.json, 8/8 PASS. M,W: xplat-send-20261113T044855Z.json, 7/8 PASS; refuse W>M failed once ("No machine has that name or ID" for lanex2-m although `pb machine list --q lanex2-m` lists it a moment later) and PASSED on an immediate rerun (14 s).
Covers both ways: ten files equal sha256 + eleven refused naming the limit of 10; no setuid/group/other/exec from sender modes 4755/0755/0777/0600, Windows receiver owner-only ACL; folder and symlink refused naming path and what to do; `pb inbox set` refuses a relative path and the other OS's form naming it, accepts `~`, spaces/non-ASCII, a second drive (D: = a 256 MB VHDX mounted on Victus for this check, removed afterwards) and `\\localhost\c$\...`, each delivered.

## Resume (by hand: 400 MiB random file made on the sender, `pb send`, 2 s later `pb service restart` on the receiver, sha256 compared)
W>L restart rc 0 at 5 s, send rc 0 after 176 s, equal sha256; L>W 271 s; M>W 65 s; W>M 59 s: all equal sha256.

## Redirected folder (OneDrive-style)
Junction `C:\Users\Pujan\OneDrive - X15 Ünï` -> `...\x15\onedrive-real`; `pb inbox set "C:\Users\Pujan\OneDrive - X15 Ünï\Inbox files"` rc 0, `pb inbox path` shows it; send from L and from M lands in the real folder with equal sha256; `pb inbox reset` restores `C:\Users\Pujan\Paperboat\Inbox`; junction removed.

## Team file request to/from W: NOT PASSED
Team paperboat-smoke-365ac02bcda9 (fleet account owner): member account B (container xpe-lb2) invited and accepted, lanex2-l/-m/-w shared and granted `files` to B. B `pb --workspace <team> send` to W, L and M: the request reaches the owner, `pb inbox approve` (run on L, M or W) rc 0, then B's send fails with HTTP 429 `sdk_limit_reached` (rate_limited, stage http_machines) on the descriptor request, in all four cases and on every retry minutes apart, while B's live operations are 0-16 (limits 32/token). Server-side diagnosis in progress.
