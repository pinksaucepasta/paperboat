# X-24 in real GUI terminals and the key list, lane R-X-24B (2026-11-14..16 IST)

Fleet lanex2 (`lanex2-l2` Linux container on anvit, `lanex2-m` macOS 26 pbmac VM, `lanex2-w` Windows 11 victus). Build **dev-08d3c4462** on all three
(FLEET3, `pb version` read on each, exec 6/6) for everything below unless a line names another build.

## Key list, all six directions (`tools/device-smoke/xplat_keys.py`, new)

A pexpect pty runs `pb connect <target>` on the client, a raw-mode reader on the target (python, `ReadConsoleW` on Windows) logs the bytes its stdin got.
35 checks per direction: arrows, Home, End, Delete, Insert, PgUp/PgDn, F1-F12, Alt-arrows, Ctrl-Right, Shift-Tab, Alt-b, SGR mouse press/release/wheel,
bracketed paste with LF, UTF-8 (wide characters and an emoji), then Ctrl-Z (Stopped on bash, suspended on zsh, `^Z` on cmd) and the shell still answers.

| direction | result | file |
|---|---|---|
| L>W, W>L | 35/35 each | keys-L-W-dev-08d3c4462.json |
| M>L, M>W | 35/35 each | keys-M-pairs-first-run-dev-08d3c4462.json |
| L>M, W>M | 35/35 each (first run 34/36: the test looked for bash's "Stopped", zsh says "suspended"; harness fixed) | keys-L-M-W-M-rerun-dev-08d3c4462.json |

## GUI terminals

| client terminal | targets | what was driven | result |
|---|---|---|---|
| Terminal.app (Mac VM, real window, System Events key presses) | Linux, Windows | info (TERM/COLORTERM/LANG, wide characters), vim, less, 24-bit colour, edit on W, chcp 437, Ctrl-C under an output flood, 20x5 and 300x100 windows, bar after vim | all pass; after-vim bar bug found and fixed, see below |
| iTerm2 3.7.7 (installed user-scoped in ~/Applications on the Mac VM) | Linux, Windows | same list (iTerm keeps its own 80x25 window and asks before resizing it) | pass: vim/less restore, bar single row, flood stops (frames identical), edit on W |
| gnome-terminal 3.52 (Xvfb, xdotool X11 key events, on lanex2-l2) | Windows, macOS | info/UTF-8 (CJK font installed), vim and bar after vim on the Mac target, less, colour, edit on W, chcp 850/65001/437, Ctrl-C under flood (frames equal 0.5 s after), 20x5 and 300x100 | pass |

## Product bugs the GUI terminals found

1. **9c50cab72** `pb connect`: the prompt came back on row 1 after vim/less on every terminal. 8d78dbd88 (X-25) wrote ESC[r before ?1049h; ESC[r homes the cursor,
   so the terminal saved the cursor at (1,1). Bytes captured from a client: ESC 7, bar, ESC 8, ESC[r, ESC[?1049h. Fix: save/restore around the margin reset.
   Test TestScrollMarginIsClearedBeforeTheChunkThatEntersTheAlternateScreen (fails on d47f51328).
2. **432169341** Terminal.app: after vim the screen scrolled up one line, a stale bar row stayed above the live one and the prompt landed a row below its text
   (`terminal-bug/after-vim-2.jpg`). Cause isolated by replaying the bytes captured from the vim session (`typescript.bin`, made with script(1) in the Terminal.app window)
   into a Terminal.app window with no pb: the prefix + ESC[?1049h + ESC[?1049l alone scrolls when the bar row fills the last column
   (`replay-prefix-1049-100col-bar.bin`, `screen-100col-bar-scrolls.jpg`) and does not when the bar is 99 columns (`replay-prefix-1049-99col-bar.bin`, `screen-99col-bar-no-scroll.jpg`),
   with or without ESC[K; gnome-terminal and iTerm do not scroll. Fix: Apple_Terminal gets a bar one column narrower. Test TestAppleTerminalBarLeavesTheLastColumnFree (fails before).

## Not done

- REC indicator in the GUI terminals: it needs a team-owned recorded terminal and a team-member client on the Mac/Linux GUI; the fleet account has no team. The draw itself is
  the same bar code covered by unit tests and by the 2026-11-13 pty runs (Linux 100 cols, macOS 40 cols, Windows 20x10 clients).
- Windows Terminal and legacy conhost on victus were done by R-X-24 (dev-e4515714f); victus has no logged-in desktop session now, so the firewall-dialog check for dda3519a0/49aedcff2
  could not be watched on the desktop: the live pb.exe of dev-08d3c4462 has an inbound allow rule "Paperboat pb.exe" (all profiles) for bin\pb.exe and its version directory.
- gnome-terminal 20x5 after an output flood: xdotool lost window focus in that tiny window (harness), the same step passes without the flood and the pexpect pty run (input after flood+resize delivered) passes.

## Fix verified in the real window (dev-432169341, lanex2-m)

Terminal.app to Linux: BEFORE / 29 100 / vim line / echo AFTER / prompt in order, one bar row, no scroll (`terminal-bug/fixed-dev-432169341-after-vim.jpg`); less restores
Ctrl-C under the flood leaves `^C` and the prompt, two frames 0.7 s and 1.5 s after are identical in the terminal area
(`terminal-bug/fixed-dev-432169341-flood-stopped.jpg`). Terminal.app to Windows: edit draws and restores, one bar row.
