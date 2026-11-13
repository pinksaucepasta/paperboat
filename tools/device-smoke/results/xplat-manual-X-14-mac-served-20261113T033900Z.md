# X-14 console Files download of a macOS-served Inbox (by hand), and the 'sp ' send gap

Date: 2026-11-13 ~09:10 IST. Fleet: lanex2-l and lanex2-m `pb version` dev-e4d2cc0f8 (cycle 2, contains 113b4c9bc). lanex2-w was offline during this run (its runner winpool-3 is gone), so the Windows browser ran on a browser-only runner (winpool-11, Chrome); only the browser's behaviour matters for the download name.

## Seeding the Mac Inbox (L -> M, `pb send` as the fleet user, nothing else on the Mac)
Names of xplat_send.py `unix_names()` sent from lanex2-l to lanex2-m (machine_9e1851b7): a:b.txt, 'trail.', CON.txt, q"u*?.txt, A.txt, a.txt, NFC, NFD, -lead.txt, 'sp ace and üñí.txt' arrived (stored under their own names; repeated sends get ' (2)' reported). 
- `pb send 'sp ' --to lanex2-m` -> rc 1: `pb: The Team Inbox request is invalid. (invalid_team_inbox_request). Support reference: support_d9cf668c-…` : NO file name and NO reason (row requires both). Same on 2026-11-12 (results/xplat-send-20261112T105602Z.json counted it as PASS because the harness accepted the word "invalid"). Cause (Haiku diagnosis, verified in code): server/internal/teaminbox/service.go:255 refuses surrounding white space, backslash, CR, LF for every receiver, transfername.Refusal only checked them for a Windows receiver. Fixed in commit 4f33e1d43 (sender refuses by name and reason for every receiver) + harness now requires the file name in a refusal; to be verified on the next cycle build with `pb send 'sp ' --to lanex2-m` from L.
- The 250-byte name sent once more to lanex2-m timed out after 120 s on the sender (the same name arrived on the first send of this run); not reproduced yet, one line in failures.md.

## Console Files download of lanex2-m's Inbox, three browsers (console_download_names.mjs, ONLY=names)
Command: `ORIGIN=<console> STATE=<fleet storage state> node console_download_names.mjs machine_9e1851b7-… <out.json> <OS>`.
| Browser / OS | result file |
|---|---|
| Chromium headless shell 1248, Linux (oracle) | x14-console-download-mac-served-linux-20261113T033900Z.json |
| Chrome (channel chrome), Windows (runner winpool-11) | x14-console-download-mac-served-windows-20261113T033900Z.json |
| Chromium headless shell 131, macOS (pbmac VM, browser only, removed afterwards, idle CPU checked) | x14-console-download-mac-served-macos-20261113T033900Z.json |

Result: 21 Inbox items (the 10 seeded names plus the 250-byte name, their ' (2)' copies and older copies): all three browsers announced the same file name and received the same byte count for every item: 'trail.'->'trail_', 'CON.txt'->'_CON.txt', 'a:b.txt'->'a_b.txt', q"u*?.txt->'q_u__.txt', the others unchanged (NFC/NFD kept, 250-byte name kept). Matches the Linux-served result of 2026-11-13 (xplat-manual-X-14-console-20261113T023557Z.md).
