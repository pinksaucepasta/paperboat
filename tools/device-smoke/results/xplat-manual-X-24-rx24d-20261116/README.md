# X-24: REC indicator in real GUI terminals, round 3, lane R-X-24D (2026-11-16 IST)

Follows R-X-24C (`../xplat-manual-X-24-rx24c-20261116/README.md`). Two product bugs from that round fixed and checked here, then the GUI terminals
re-run on the final build **dev-069460edd** (= main 069460edd, built on hermes; `pb version` read on rx24d-g, rx24d-l, rx24d-w2 and the pbx24 user of pbmac2).

## Setup

- Account: the dev test account, team `paperboat-smoke-paid` (recording key set up, team plan). Own records only, all named `rx24d-`:
  `rx24d-g` (client container on hp: gnome-terminal under Xvfb, xdotool; also the place `pb connect` runs for the Mac windows),
  `rx24d-l` (Linux target, container on hp), `rx24d-m-2` (macOS 26.6.2 arm64 target: second macOS user `pbx24` on the pbmac2 VM, its own install and daemon),
  `rx24d-w2` (Windows Server 2025 runner, winpool-2). All four are team-owned with terminal/exec/files granted to the one account.
- Terminal.app and iTerm2 3.7.7 are real windows on the fleet Mac VM (lanex2-m, CGEvent key and mouse events, `screencapture`). `pb connect` runs in `rx24d-g` and is
  reached over `ssh -t` with `TERM_PROGRAM` set to the app's own value: the window is the emulator, the bytes are the real bar bytes. Gap: the client `pb` is the
  Linux one, not a macOS `pb` (macOS-specific client behaviour is covered by the unit tests, R-X-24B's Terminal.app run on a macOS client was without REC).
- Per run: attach to the target, `pb recordings start` from a second process (REC and the "your team records this session" notice appear), vim (edit on Windows),
  typing, window made smaller and larger (Terminal.app: CSI 8;r;c t written to the tty; iTerm2: dragging the window corner), leaving the application, closing the
  client, attaching again with `pb connect --session <printed id>` (REC again).

## Bugs fixed in this round (main 069460edd, CI run 43852514901 green)

| commit | what the window showed | cause | test (fails before) |
|---|---|---|---|
| 8d3259489 | gnome-terminal: a stale copy of the "connecting" bar in the middle of the screen after the window grew inside vim | VTE pushes the screen into its scrollback on ESC[2J, the bar row with it, and pulls it back when the window grows (reproduced with bytes: bar, ESC[2J, vim, resize, leave); the attach clear, the exit clear and a remote clear all did it. Also a blank row in vim after a taller window: the old bar row was erased after vim repainted it | TestBarRowIsErasedBeforeAClearOfTheMainScreen, TestTallerWindowInsideAFullscreenApplicationErasesNothingOnTheAlternateScreen |
| 839c82550 | `pb recordings start` printed `terminal_offline_<uuid>`; `pb connect --session <it>` failed with "Paperboat could not finish the command" | such a session exists only on its host (opened without Paperboat); connect looked it up in Paperboat's list and the plain error named no step | TestResolveReopensAnOfflineSessionByIDWithoutAskingPaperboat, TestUnknownTerminalSessionErrorNamesTheLookupAndTheRecovery. Live: connect by that id re-opened the session and replayed the typed marker; unknown id prints "terminal session ... was not found in the sessions Paperboat lists for this machine; list them with `pb session list` ..." |
| 069460edd | iTerm2: copies of the REC bar (and vim's status line) in the main screen after a resize inside vim and leaving it | iTerm2 saves a cleared alternate screen into its scrollback and a recorded session keeps the bar on it; the erase before a clear now also applies while the bar is visible on the alternate screen | TestBarRowIsErasedBeforeAClearOfTheMainScreen/recorded_alternate_screen |

## Result on dev-069460edd (contact sheets in `final/`: sheet1/A = REC, vim, window changes, sheet2/B = after leaving, closed, reattached)

| client window | target L (Linux) | target M (macOS) | target W (Windows) |
|---|---|---|---|
| gnome-terminal 3.52 | PASS `G-L-sheetA/B` | PASS `G-M-sheetA/B` | PASS `G-W-sheetA/B` |
| Terminal.app | PASS `T-L5-sheet1/2` | PASS `T-M2-sheet1/2` | PASS `T-W5-sheet1/2` |
| iTerm2 3.7.7 | **GAP** `I-L6-sheet1/2` (REC and bar correct throughout; after leaving vim the main screen holds remnants of the alternate screen) | PASS `I-M2-sheet1/2` | PASS `I-W5-sheet1/2` |
| Windows Terminal, conhost | not re-run here (no Windows desktop session); R-X-24C passes on dev-49332aea7 stand | not re-run | not re-run |

Pass = REC and the notice at attach, REC on the live bar in vim/edit while typing, after a smaller and a larger window, after leaving the application one bar row and
the prompt in order and no stale copy, the closed client, and the re-attach by the printed id shows REC.

## Gaps

- iTerm2 against the Linux target (Debian vim 9.1), 3 of 3 runs (`I-L4`, `I-L5`, `I-L6`): after a resize inside vim and leaving it, rows of the alternate screen stay in
  iTerm2's scrollback and reappear above the prompt: vim's own "-- INSERT --" status rows and copies of the REC bar at the other widths. The bar itself and REC are correct
  in every frame. **Control without pb** (`final/I-CTL-plain-vim-no-pb-after-vim.jpg`: plain `ssh -t ... vim`, same shrink and grow, no `pb`): the same vim rows are left, so
  this is iTerm2's "save lines to scrollback in alternate screen mode" with that vim, not the bar. The bar rows are extra rows of that screen; pb cannot keep REC on
  the alternate screen and keep it out of iTerm2's history. The macOS target (Apple vim) and the Windows target (edit) leave nothing.
- Windows Terminal and conhost were not re-run on the final build (hosted runner without a console session); conhost against the macOS target has still not been run.
- pbmac2's macOS user `pbx24` has a degraded local daemon (the login keychain is locked in its launchd session); the machine is online and works as a target. Not a product check.
- The fleet Mac showed a macOS "Allow pb to find devices on local networks?" dialog that took keyboard focus; it was answered Allow.

## Records (revoke/delete by exact id)

rx24d-g machine_9df99f54-826e-4ba2-800e-1540b1bbe43c, rx24d-l machine_0ad627ec-1f17-42b8-adf6-1a69cac800ef, rx24d-m-2 machine_ce9e5295-290b-478b-9e4c-673b912638e0,
rx24d-w2 (winpool-2), orphans from failed pairs: rx24d-w machine_07ebdf7c-940b-4435-a08a-e1aaa29f0963, rx24d-m machine_b9cd70a5-4958-4c53-bb4b-7f03a9db7034.
