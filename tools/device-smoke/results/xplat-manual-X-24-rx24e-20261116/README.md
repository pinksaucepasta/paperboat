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

## W cell (Windows Terminal, conhost on the real Windows PC victus = lanex2-w): ticked

Client windows are real windows on victus's interactive desktop (RDP session; `driver-rx24e-win.ps1`: a scheduled task in the console session, keys by
WriteConsoleInput into the window's console, window resized with SetWindowPos, `CopyFromScreen` + console buffer dumps). The driver reads and types into the console of the
*newest* window (`conpids.txt`: first attach and re-attach use different console pids); an earlier version of it read the old window's leftover prompt after the
first client was closed and logged a Windows re-attach bug that did not exist (retracted in failures.md; force-kill with TerminateProcess then
`pb connect --session` replays and takes input, tried on a personal target).

Builds (`pb version` read on every machine, `runs-final.txt`): victus (client, fleet instance) dev-485e1f23e, rx24e-l dev-485e1f23e, lanex2-m dev-485e1f23e.
Targets: `rx24e-l` (Linux container on pbvm2, team-owned in rx23-team so recording works) and the fleet Mac lanex2-m (personal: no recording there, so REC on a macOS target was shown
on the first round, below).

Per run: attach (`pb connect <t> --workspace rx23-team`), `pb recordings start` from a second process (REC + the "your team records this session" notice), vim with typing,
window made smaller (640x360 px) and larger (1000x640), leaving vim, then the window is closed (WM_CLOSE) and the session attached again by id (`pb connect <t> --session <id>`),
with a command typed 3 s after the attach.

| client window | Linux target rx24e-l (dev-485e1f23e, REC) | macOS target |
|---|---|---|
| Windows Terminal | PASS `final/wt-el4-sheetA|B.jpg`: REC + notice, REC on the bar in vim while typing, after the smaller and the larger window, one bar row and the prompt after leaving vim, re-attach by id shows the replay with REC and `echo REATTACH_OK` runs | PASS: REC through attach, vim, resize, leave on dev-d9d082603 (`final/wt-em-sheetA|B.jpg`, recorded team Mac, no stale bar rows; the re-attach window there is the second window in the frames); clean re-attach by id on dev-485e1f23e against lanex2-m (`final/wt-m4-sheetA|B.jpg`, conpids differ, typed command runs) |
| conhost | PASS `final/ch-el4-sheetA|B.jpg` (same steps) | PASS: same on dev-d9d082603 (`final/ch-em-sheetA|B.jpg`) and clean re-attach on dev-485e1f23e (`final/ch-m4-sheetA|B.jpg`) |

Why the macOS REC round on dev-d9d082603 still counts: the commits between it and dev-485e1f23e touch `cli/internal/hostruntime/server` only for ENV resolution (276fad35b);
no status bar, session, terminal or connect code (`git log d9d082603..485e1f23e -- cli/internal/statusbar cli/internal/session cli/internal/terminal cli/cmd/pb/connect_run.go`).
The Mac runner for a recorded macOS target on the final build did not start in 15 min (GitHub macOS queue, macpool-3), the earlier recorded Mac (macpool-2) had to be stopped for the release build.

### Enter typed right after attach (R-X-26-N report, Windows target)

Not reproduced on dev-485e1f23e: `enter-after-attach-linux-client.py` (Linux client rx24e-l) and `enter-after-attach-mac-client.py` (fleet Mac client) attach to the Windows
target lanex2-w with `--session`, type `echo <tag>` + Enter 0.1, 0.5 and 3 s after the attach and check the command ran: 3 of 3 at each gap from both clients;
`enter-before-attach.py` types at 0.2 s after starting `pb connect` (before the shell attaches): the command runs (Linux client; with and without `--session`).
`session.HoldInput` (6165b50ea) puts the tty in raw mode before connecting, which is what makes CR survive. A blackholed control plane for the first 6 s did not change that.
The four Windows-window runs type 3 s after the attach as well. (A first version of my check sent Ctrl-U between tries and left a stray byte in the cmd line: harness, not product.)
