# X-24: GUI client terminals, final cells, lane R-X-24E (2026-11-16 IST)

Continues `../xplat-manual-X-24-rx24d-20261116/README.md`.

## M cell (Terminal.app, iTerm2): ticked

Coordinator decision (2026-11-16 IST): behaviour identical to the terminal's native behaviour is a pass.
Row re-read: every named part for a macOS client passed on Terminal.app and iTerm2 (R-X-24 11-14, R-X-24B, R-X-24C, R-X-24D on dev-069460edd):
TERM/COLORTERM/LANG, wide characters, alternate screen in vim/less/edit, colour, keys, mouse and paste, Ctrl-Z, Ctrl-C under a flood, 20x5 and 300x100,
and `--status-bar` with REC through attach, vim/edit with typing, a smaller and a larger window, leaving the application and a re-attach by id,
against Linux, macOS and Windows targets (Terminal.app x {L,M,W} PASS, iTerm2 x {M,W} PASS, iTerm2 x L PASS apart from the point below).

The one open point of R-X-24D: iTerm2 against the Linux target leaves rows of the alternate screen in its scrollback after a resize inside vim and leaving it.
Control without pb (`../xplat-manual-X-24-rx24d-20261116/final/I-CTL-plain-vim-no-pb-after-vim.jpg`: same shrink and grow, plain vim/edit, no pb): the same rows
("-- INSERT --" status rows) stay in iTerm2's scrollback, so it is iTerm2's "save lines to scrollback in alternate screen mode". With pb the saved screen also holds
the bar row, so copies of the bar sit in the scrollback beside those rows. They are history lines above the prompt: the live bar and REC are correct in every frame, nothing
is overdrawn or wrapped. Commits after dev-069460edd (`git log 069460edd..HEAD -- cli/internal/terminal cli/cmd/pb/connect_run.go`) touch none of the terminal/bar code.

## W cell (Windows Terminal, conhost on the real Windows PC victus = lanex2-w): in progress

Client windows are real windows on victus's interactive desktop (RDP session, `driver-rx24e-win.ps1`: a scheduled task in the console session, keys by
WriteConsoleInput into the window's console, window resized with SetWindowPos, `CopyFromScreen` + console buffer dumps; headless conhost for the task itself).
Targets are team-owned throwaway machines of the fleet account's team rx23-team (recording needs a team): `rx24e-l` (Linux container on pbvm2) and `rx24e-m`
(macOS 26.7.2 arm64 runner, macpool-2), `pb connect <t> --workspace rx23-team`, `pb recordings start` from a second process, vim on the target.

Run on dev-d9d082603 clients/targets (pb version in `runs.txt`; sheets `final/<client>-<target>-sheetA|B.jpg`, A = REC, vim, smaller window, larger window; B = after leaving vim, prompt, re-attach):

| client window | Linux target (rx24e-l) | macOS target (rx24e-m) |
|---|---|---|
| Windows Terminal | REC + team notice at attach, REC on the bar in vim while typing, smaller (949x635 px start, 640x360) and larger (1000x640) window, one bar row and the prompt after leaving vim | same |
| conhost | same (816x519 px start) | same |

Open for the W tick: the re-attach by session id (`pb connect <t> --session <printed id>` after the first client was killed with TerminateProcess) replays the
session with REC on the bar and then exits with code 0 and nothing on stderr within 3 s, on all four runs (the Linux-client equivalent, SIGKILL then re-attach, works;
sessions stay `detached`, not `closed`). Not yet isolated: whether the force-kill (real users close the window), or the Windows client, is the cause.
From 19:06 IST the team machines were unreachable from every client after prod server .34 (FIX-TEAMREACH), so the window-close variant and a clean re-run wait for that fix.
