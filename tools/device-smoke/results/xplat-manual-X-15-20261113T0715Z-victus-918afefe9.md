# X-15 Windows parts, final pass, 2026-11-13 12:00-13:10 IST

`pb version` of lanex2-l (Linux container on hp), lanex2-m (macOS, pbmac VM), lanex2-w (real Windows 11, Victus), member sender xpe-bl (Linux container, second account): all dev-918afefe9 (main 918afefe9), server 2026.11.13.13.

## Scripted: tools/device-smoke/xplat_send.py --only batch,modes,refuse,inboxset
L,W: results/xplat-send-20261113T063700Z.json 8/8 PASS. M,W: results/xplat-send-20261113T064111Z.json 8/8 PASS.
Both ways each: ten files equal sha256 and eleven refused naming the limit of 10; no setuid/group/other/exec from sender modes 4755/0755/0777/0600, Windows receiver owner-only ACL; folder and symlink refused naming path and what to do; `pb inbox set` refuses a relative path and the other OS's form naming it, accepts `~`, spaces/non-ASCII, a second drive (D: = a 256 MB VHDX mounted on Victus for the check, removed) and `\\localhost\c$\...`, each delivered, `pb inbox path` shows it.

## Size limit, by hand (2,000,000,001-byte file on the sender, `pb send` in Automatic mode)
L>W, W>L, M>W, W>M: rc 1 "This transfer is 2.0 GB. Automatic sends up to 2 GB. Use --unlimited to send directly or through your own relay. Nothing was sent."; nothing in the receiver Inbox.

## Resume, by hand (400 MiB random file made on the sender, `pb send`, 2 s later `pb service restart` on the receiver, sha256 compared): all equal
W>L 149 s, L>W 168 s, M>W 67 s, W>M 57 s (restart rc 0 at 5-8 s each).

## Redirected folder (OneDrive-style)
Junction `C:\Users\Pujan\OneDrive - X15 Ünï` -> real folder; `pb inbox set` of `...\Inbox files` inside it rc 0; sends from L and from M land in the real folder with equal sha256; `pb inbox reset` restores the default; junction removed.

## Team file request received on W, and sent from W
Team paperboat-smoke-365ac02bcda9 (fleet account owner), member account B on xpe-bl (invited and accepted for the check, shared lanex2-l/-m/-w with the files capability; all undone afterwards: machines unshared, B removed). B `pb --workspace <team> send` of a 1 KiB random file, approved with `pb inbox approve` on another fleet OS, lands with equal sha256:
receiver W approved on L: PASS; receiver W approved on M: PASS; receiver L approved on W: PASS; receiver M approved on W: PASS.
Found on the way and fixed (918afefe9): before it, every one of these ended in `sdk_limit_reached` after ~2 min: the destination daemon refused the member's credential as credential_malformed (the server signs `transfer_direction: send` for API-token sessions; the host's claims struct had no such field), and the sender renewed the descriptor after each refused stream until the per-token operations cap. Test: TestVerifierAcceptsTheSendTransferDirectionOnly (CI run 43376602470). Not fixed: the renewal opens one access session per renewal without a cooldown (stops at the cap).

## Earlier first-pass lines superseded
The W>L "resume stall" (xplat-manual-X-15-20261113T0335Z-windows.md) was the harness uploading 60 MiB to a Windows sender over ssh, not the product.
