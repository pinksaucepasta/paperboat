# X-24 by hand in real GUI terminals, lane R-X-24 (2026-11-14 IST)

Fleet lanex2 (account fresh, owned by lane X): `lanex2-l2` Linux container (anvit, arm64, target has no LANG), `lanex2-m` macOS 26 (pbmac Tart VM),
`lanex2-w` Windows 11 (victus). Build under test **dev-e4515714f** on all three (FLEET3, `pb version` read on each, exec 6/6, doctor ok).
The Ctrl-C fixes 05c05d6db, 0bca2f0d4, b8841540e, 653bc7e1a were in the fleet's earlier build dev-5228278ac; c700dfae5 was only on branch `fix-winctrlc`
(never on main), so it was landed first as a1df0ffbe (tunnel + config tests green on anvit) and installed as part of dev-b6a4df91c.

## Ctrl-C under an output flood (six directions, `flood-six-directions-e4515714f.log`)

Client in a pexpect pty over ssh (docker exec / ssh -tt), `yes`-style flood, byte counter after the Ctrl-C byte:

| direction | stop after Ctrl-C |
|---|---|
| L>M | 0.0 s PASS |
| L>W | 0.4 s PASS |
| M>L | 0.1 s PASS |
| M>W | 0.5 s PASS |
| W>L | 2.3 s (494 KB still arrived) FAIL in this harness |
| W>M | 1.3 s (287 KB still arrived) FAIL in this harness |

The two W-client directions go through sshd's ConPTY and the ssh client, which buffer what pb already wrote; the user's own Windows clients are a console window.
Measured there, on the victus desktop (RDP session, the real window; Ctrl-C is a real console key event with the Ctrl state, injected with WriteConsoleInput
into the window's console; screenshot frames ~0.3 s, 0.6 s, 0.9 s, 1.9 s after the key): the four frames are byte-identical (`win/*/flood-md5.txt`) for
conhost>L, conhost>M, Windows Terminal>L, Windows Terminal>M, so the flood had stopped by the first frame (<= ~0.4 s), and the prompt answers `echo FLOOD_STOPPED_OK`
(`win/*-full/flood-prompt.txt`). Terminal.app>L (real window, real Ctrl-C key through System Events): frames 0.7 s and 1.5 s after the key identical (SAME md5).

## UTF-8 locale on a target with no LANG

`win/*/info.txt` (typed into the window; the text dump is read from the console buffer): Linux container target `TERM=xterm-256color COLORTERM=truecolor LANG=C.UTF-8 LC_ALL=`
(`locale charmap` UTF-8, `日本語 ✓ é` echoed back whole); macOS target `LANG=en_US.UTF-8`. Both from Windows Terminal and from conhost. Terminal.app>L: same lines (mac/info.jpg).

## Real GUI terminals

Windows (victus desktop, 100x30 window; `win/<client>-<target>-full/`): vim opens in the alternate screen with the typed text and restores the prompt (vim.txt, vim-restored.txt),
less pages and restores (less.txt, less-restored.txt), 24-bit and 16-colour output (colour.txt), the bar never wraps. Windows Terminal and legacy conhost (`conhost.exe cmd.exe`), each to the
Linux and the macOS target. Sizes 20x5 and 300x100 (`*-small`, `*-big`): `stty size` 99 300 on the big window (bar row reserved), a one-row bar that fits ("connected d") in 20x5.
Code pages (`conhost-l2-chcp437|850|65001`): the window's input code page 437, 850 and 65001; typed `日本語 ✓ é` reaches the remote byte for byte
(`cat -v`: M-fM-^WM-%…) and the echo comes back correct in all three.

Typing into the windows: conhost and Windows Terminal both take real console input events (WriteConsoleInput on the window's console) because the ComfyUI launcher cmd the user
starts at logon holds the foreground and the keyboard focus in a scripted session; a real key path was shown separately in Windows Terminal earlier today (SendInput, dev-b6a4df91c).
Mac: Terminal.app driven with System Events (real key presses, Cmd+V paste for text, Ctrl-C key code), screenshots with `screencapture`.

## Product bugs the real windows found (each fixed with a test on main)

| bug | commit | test |
|---|---|---|
| Windows: `pb connect` from a normal console ended with "Access is denied" writing config.json.workspaces after an elevated ssh session had created it (default token DACL without the user) | 09c0b2e0f | TestWorkspaceRecordDirectoryHasProtectedUserACL (windows) |
| Windows conhost: status bar and remote colours printed as literal `←[...m` (nothing enabled VT output) | ae6fe6ebd | TestEnableVirtualTerminalOutputTurnsOnAndRestores (windows) |
| macOS Terminal.app: the host's in-band resize broadcast (ESC[8;rows;colst) shrank the window to 100x1 (only the Windows client filtered it) | e48941160 | TestRunDropsTheHostsInBandResizeRequest |
| Terminal.app ignores CSI s / CSI u: after a bar draw the cursor stayed on the bar row and the prompt overwrote the bar after vim | f9119cf55 | TestAppleTerminalSavesTheCursorWithDECSCAndDefersToTheRemotesSave |
| console output code page: set to UTF-8 for the session (hardening; the mojibake first seen was my script's own encoding, not shown to be a pb bug) | e4515714f | TestUseUTF8OutputCodePageSwitchesAndRestores (windows) |

Noticed, not fixed (outside this row): every new pb build asks for Windows Firewall "allow pb.exe" on the Windows desktop (rules pile up per release path) and for the macOS
"Allow pb to find devices on local networks" prompt; both appear over the first run of each build.
