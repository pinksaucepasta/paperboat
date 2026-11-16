# X-24: REC indicator in real GUI terminals, lane R-X-24C (2026-11-16 IST)

Gap closed here: the REC indicator in the GUI terminals while the target terminal is recorded (the rest of X-24 is in
../xplat-manual-X-24-rx24b-20261116/README.md).

## Setup

- Account: the dev test account, own records only, all named `rx24c-`: `rx24c-l`, `rx24c-l2` (Linux containers on hp, Ubuntu 24.04), `rx24c-m`
  (macOS 26.7.2 arm64 runner, macpool-1), `rx24c-w` (died with its runner) and `rx24c-w2` (Windows 11 runner, winpool-1).
- Recording: the deployed server (2026.11.13.27c) predates personal recordings (f24ee62dd), so the machines were moved into the existing
  paid test team `paperboat-smoke-paid` (`pb team machine transfer-to-team`, terminal/exec/files granted to the one account only) and the
  team's existing recording key was used. The machine takes the team's recording policy up to 5 minutes after the transfer
  (`recordingcontrol.policyInterval`); `pb recordings start` says "recording is not available on this machine" until then.
- Client terminals, real windows on a desktop session, driven by key events and read from screenshots:
  gnome-terminal 3.52 under Xvfb (xdotool, container `rx24c-l`), Terminal.app and iTerm2 3.7.7 on the macOS runner (CGEvent key/mouse events,
  `screencapture`), Windows Terminal and legacy conhost on the Windows runner (`SendKeys` from a scheduled task in the console session,
  `CopyFromScreen`). Recording is started from a second process on the client computer with `pb recordings start` while the window is attached
  with `pb connect` (the way a user starts it).
- Build under test: **dev-49332aea7** (`pb version` read on rx24c-l, rx24c-l2, rx24c-m, rx24c-w2) = main with the four fixes below.

## What each window was driven through

1. attach, `pb recordings start`: REC in the bar (and the one-line "your team records this session" notice);
2. a full-screen application on the target (vim on Linux and macOS, `edit` on Windows): REC stays;
3. typing in it (vim insert mode, `edit` text);
4. resize the window larger and smaller while the application runs: REC on the live bar;
5. leave the application: REC and the prompt;
6. reconnect: gnome-terminal client machine loses its network for 8 s (`docker network disconnect`) and the bar shows
   "Reconnected after no reply from <target>" with REC; then the client is closed and attached again to the recorded session: REC.

## Result on dev-49332aea7 (contact sheets in `final/`)

| client window | target L (Linux) | target M (macOS) | target W (Windows) |
|---|---|---|---|
| gnome-terminal | PASS `F-G-L-L-*` | PASS `F-G-L-M-*` | PASS `F-G-L-W-*` |
| Terminal.app | PASS `F-A-M-L-*` | PASS on the build before the colour fix (`a0770b2ec/A-M-M-*`, REC everywhere, bar coloured by vim) | not run |
| iTerm2 | prompt only (`final/F-IT-M-L-1-rec.jpg`) | not run | not run |
| Windows Terminal | PASS `F-A-WT-L-*` | PASS `F-A-WT-M-*` | PASS `F-A-WT-W-*` |
| conhost | PASS `F-A-CH-L-*` (reattach by id failed, see below) | not run | PASS `F-A-CH-W-*` (second window attached to the recorded session shows REC: `final/F-A-CH-W-second-window-attached-REC.jpg`) |

## Product bugs the real windows found (each fixed with a test that fails before)

| commit | what the window showed | cause | test |
|---|---|---|---|
| bc2d21991 | REC and the whole bar vanish while the target runs vim/edit (`before-fix/*-vim-REC-hidden.png`, gnome-terminal and Terminal.app) | the default full-screen mode suspends the bar on ESC[?1049h, REC with it | TestRecordingKeepsTheBarInFullscreenApplications |
| 0361c7c1b | macOS target, vim insert mode: bar row blank until the window is resized | ESC[2J arrived in a chunk that ended inside the next escape sequence; the owner loop forgot the erase and never asked for a redraw (reproduced from captured bytes: `script -T`, 1048+1781+52 byte chunks) | TestEraseInAChunkEndingMidSequenceStillRepaintsTheBar |
| f0c2b8f1a | bar green/blue under vim, white with blue text under `edit` | default theme painted reverse video only, which swaps the application's colours | TestBarStartsFromDefaultColoursAndRestoresTheApplications |
| 70b0fa4f6 | a stale bar row left above the prompt after the window grew | nothing erased the row of the old height | TestTallerWindowErasesTheOldBarRow (unit level; see gaps) |

CI: runs 43802734403, 43806225077, 43809879457 (`--- PASS` lines for the four tests in the job logs).

## Gaps

- gnome-terminal (VTE) keeps a stale copy of an earlier bar row in the middle of the primary screen after the window was resized while a
  full-screen application ran (VTE re-flows the saved primary screen; the row number pb erases is not where it ended up; the copy can wrap
  at a narrow width). The live bar with REC at the bottom is correct in every frame. Terminal.app, Windows Terminal and conhost showed no stale row.
- gnome-terminal against the macOS target (zsh) after resizing inside vim: the first prompt after vim can land under the bar for a few seconds.
  Not seen with Linux or Windows targets or from any other window.

## Not done, and why

- Terminal.app against the Windows target and iTerm2 against all three targets (window sheets, vim/edit, resize, reattach), Terminal.app against macOS and Windows on the
  last build: the macOS runner (macpool-1) ended at its time limit in the middle of the iTerm2 run and the next GitHub macOS runner (macpool-8) stayed
  queued for 25 minutes behind other jobs (cancelled). Terminal.app and iTerm2 draw the same bar through the same code (only the `TERM_PROGRAM`
  one-column and cursor-save differences); Terminal.app against Linux passes on the last build.
- conhost against macOS, and reattach after a client was force-killed in conhost: after `Stop-Process` of a `pb connect` that had `edit` open, the console
  stayed in win32-input mode and typed keys came out as `;0;0;1_` fragments (harness artifact, shows in `final/F-A-CH-W-sheet2.jpg` third frame); the clean attach is the second-window frame above.

## Observations that are not fixed here

- `pb recordings start` can print a `terminal_offline_<uuid>` session id (the session was created by a queued operation); `pb connect --session <that id>` then
  fails with "Paperboat could not finish the command. Check the current state before retrying" (no step named). `--session <name>` works.
- Attaching with plain `pb connect <machine>` creates a new session when the earlier one has another name, so a new window is not recorded.
