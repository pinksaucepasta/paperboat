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
