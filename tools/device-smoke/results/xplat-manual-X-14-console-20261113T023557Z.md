# X-14 console Files download of odd names, browsers on Linux, Windows and macOS (by hand)

Date: 2026-11-13. Served from: lanex2-l Inbox (`pb version` dev-e4d2cc0f8, contains 113b4c9bc `transfername.Portable` for the download name); lanex2-w `pb version` dev-e4d2cc0f8. Console: the live console, signed in as the fleet account (storage state).
Items: the 12 names of xplat_send.py `unix_names()` (a:b.txt, 'trail.', 'sp ', CON.txt, q"u*?.txt, A.txt, a.txt, 250-byte, NFC, NFD, -lead.txt, 'sp ace and üñí.txt') placed in the lanex2-l Inbox (written there directly; the `pb send` side of the same names passed in results/xplat-send-20261112T105602Z.json).
Command per OS: `node console_download_names.mjs machine_7985e977-… <out.json> <OS>` (opens /files?machine=…, clicks every download link, records the browser's suggested file name, saves it into a scratch folder of that OS).

| Browser / OS | build of the browser host | result file |
|---|---|---|
| Chromium 1248, Linux (oracle, arm64) | n/a (browser only) | x14-console-download-linux-20261113T023557Z.json |
| Chrome (channel chrome), Windows (winpool-3 runner) | pb dev-e4d2cc0f8 on the runner | x14-console-download-windows-20261113T023557Z.json |
| Chromium headless shell 131, macOS 26.6 (pbmac VM, lease held) | browser only; pb on that host is dev-aee62911e (not involved in the download) | x14-console-download-macos-20261113T023557Z.json |

| Item in the Inbox | Linux | Windows | macOS |
|---|---|---|---|
| 'sp ace and üñí.txt' | 'sp ace and üñí.txt' | 'sp ace and üñí.txt' | 'sp ace and üñí.txt' |
| '-lead.txt' | '-lead.txt' | '-lead.txt' | '-lead.txt' |
| 'café-nfd.txt' | 'café-nfd.txt' | 'café-nfd.txt' | 'café-nfd.txt' |
| 'café-nfc.txt' | 'café-nfc.txt' | 'café-nfc.txt' | 'café-nfc.txt' |
| 'xxxxxxxxxxxx…(254 bytes)' | 'xxxxxxxxxxxx…(254 bytes)' | 'xxxxxxxxxxxx…(254 bytes)' | 'xxxxxxxxxxxx…(254 bytes)' |
| 'a.txt' | 'a.txt' | 'a.txt' | 'a.txt' |
| 'A.txt' | 'A.txt' | 'A.txt' | 'A.txt' |
| 'q"u*?.txt' | 'q_u__.txt' | 'q_u__.txt' | 'q_u__.txt' |
| 'CON.txt' | '_CON.txt' | '_CON.txt' | '_CON.txt' |
| 'sp' | 'sp_' | 'sp_' | 'sp_' |
| 'trail.' | 'trail_' | 'trail_' | 'trail_' |
| 'a:b.txt' | 'a_b.txt' | 'a_b.txt' | 'a_b.txt' |

Result: the browser on all three OSes announced the SAME file name for all 12 items: **True**. Every item downloaded with its bytes (the Inbox item label of 'sp ' is shown trimmed; saved as 'sp_'). Before 113b4c9bc Chrome on Windows saved 'trail.' as 'trail_' and 'CON.txt' as '_CON.txt' while Linux kept 'trail' and 'CON.txt'.
Collisions: the check script saves into one scratch folder, so on the case-insensitive Windows/macOS file systems 'A.txt' replaced 'a.txt' there (a harness artefact; a real browser download renames the second to 'A (1).txt'); the announced names are identical.
Not covered: Safari itself (no node/driver on the Mac VM); the download of a macOS-served Inbox (lanex2-m is offline in the fleet account and on dev-aee62911e, before 113b4c9bc).

## CLI names, L<->W, rerun on the same build (xplat_send.py --devices L,W --only names; results/xplat-send-20261113T024230Z.json)
lanex2-l and lanex2-w both `pb version` dev-e4d2cc0f8. PASS X-14:names:L>W and PASS X-14:names:W>L. By hand on L, `pb send <file> --to lanex2-w` of 'a:b.txt', 'trail.', 'sp ', CON.txt each ended rc 2 with, e.g.:
`pb: Nothing was sent: lanex2-w cannot store "CON.txt": CON is a reserved Windows device name, with or without an extension. Rename the file and send again.` (the usage text follows the message; cosmetic). A.txt/a.txt: a.txt arrived as 'a (2).txt', reported. 250-byte, NFC, NFD, '-lead.txt' arrived under their own names with equal sha256.
