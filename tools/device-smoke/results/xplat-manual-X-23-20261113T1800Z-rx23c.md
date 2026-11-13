# X-23 fourth pass by hand, 2026-11-13 (IST ~23:00-01:00), lane R-X-23C

Builds: console live at d138d490b's parent (no console change since 9cdace229 touches the terminal), targets on dev-b6a4df91c (= today's cli/shared, no diff to main).
Mac browser host: the pbmac VM (macOS 26.6.2, Safari, every run under `flock fleet-mac.lock`), **not automated**: a normal Safari window driven by AppleScript/System Events
(real key events), page state read with Safari's "Allow JavaScript from Apple Events" (turned on for the session, removed again). Console signed in as the fleet account.

## 1. PowerShell 7 multi-line paste to a Windows target: cause found, not our path
Question: does the console/terminal path drop or reorder the bracketed-paste markers?
- Code path (read by Haiku, then checked): console `sendPaste` (@wterm/dom input.js:430) wraps in ESC[200~ ... ESC[201~ only when the terminal core has seen ESC[?2004h from the target;
  `use-terminal-connection.ts` -> wasm `apiSendInput` -> `Sessions.Write` -> `InputJournal.Write` -> one `file.Write` into the ConPTY input pipe (`pty_windows.go`, flags 0): no hop strips, rewrites, splits or reorders ESC[200~/201~ or CR/LF.
- Byte capture on victus (Windows 11 build 26100, pwsh 7.6.6, PSReadLine 2.4.5) with a standalone ConPTY program (no Paperboat code), `Set-PSReadLineOption -BracketedPasteEnabled $true`, 3 lines pasted as
  `ESC[200~ Set-Content m1 / m2 / m3 ESC[201~`, count of lines executed before Enter:
  ConPTY flags 0: 2 of 3 ran before Enter; flags 8 (PASSTHROUGH): 2 of 3; same without the markers: 2 of 3.
  pwsh never emitted ESC[?2004h through ConPTY in this harness (the `ESC[?2004h` seen in the earlier `pb connect` capture is `pb connect`'s own local enable, as noted before),
  so the browser has no reason to wrap. With `pb connect lanex2-w` and raw bytes `ESC[200~..ESC[201~` the same 2 lines ran early (python pty driver); a `[Console]::ReadKey` loop in pwsh received only `A`,`B` for `ESC[200~AB ESC[201~`: the markers are consumed by conhost's input parser and never reach PSReadLine.
- Cause: PSReadLine has no bracketed paste on Windows (upstream PowerShell/PSReadLine issue 1471, open: it reads `Console.ReadKey`, which does not expose the markers); VS Code sends the same plain CR-separated text to pwsh on Windows (`terminalInstance.ts` sendText). Not a Paperboat bug, not fixable by moving markers.
  Windows PowerShell 5.1 behaves the same (lines run in order, each once).
- Product options (not done, needs a decision): send newlines of a multi-line paste to a Windows target as Shift+Enter (PSReadLine AddLine) so the block runs once on Enter.

## 2. Real IME composition
- macOS (done): Safari (normal window, real key events), macOS Japanese input source (Kotoeri Romaji, switched with the Globe key), console terminal to the Windows target lanex2-w (victus):
  typed n i h o n g o -> `compositionstart`, `compositionupdate` n, に, にh, にほ, にほn, にほんg, 日本語 (real kana-to-kanji conversion), Return -> `compositionend:日本語`;
  terminal showed nothing before commit, the input line then read `C:\Users\Pujan>日本語` once, and cmd.exe answered with the text once (2 occurrences in total = input line + its error message).
- Windows (done): Microsoft Edge 156 (a real, non-headless window in the signed-in desktop session 2 of winpool-1, Windows Server 2025 build 26100; Japanese language pack `Language.Basic~~~ja-JP` installed, language list en-US+ja-JP, Microsoft IME), console terminal to the macOS target lanex2-m.
  Real key events (`keybd_event`) n i h o n g o after IME-on: `compositionstart`, `compositionupdate` ｎ, に, にｈ, にほ, にほｎ, にほんｇ, にほんご, Enter -> `compositionend:にほんご`; terminal empty until commit, then `% にほんご` once on the zsh line. Screenshot of the desktop: xplat-manual-X-23-20261113T1800Z-rx23c-edge-ime-windows.png. Chrome on Windows: not run (same Chromium input path as Edge).

## 3. Safari's own 200% zoom (done)
Normal (non-automated) Safari, View > Zoom In by Cmd-+ five times: innerWidth 800 -> 400 (exactly 200%), terminal stayed attached, `echo ZOOM200` typed and echoed, terminal host 376x212 px, no horizontal page scroll;
Actual Size restored 800. (Safari's step list is 110/125/150/175/200.)

## 4. Guest REC chip on a macOS target (done)
Team rx23-team (fleet account is owner, recording enabled), macOS target rx23c-m = macpool-7 (GitHub macOS runner, pb dev-b6a4df91c, `pb doctor` healthy, enrolled with `machine add --workspace rx23-team`, shared and granted terminal+exec).
Owner: Chromium (oracle) opens the team terminal of rx23c-m, Start recording -> REC chip true, Share -> code. Guest: Safari **private window** on the Mac VM, no account (`/api/v1/me` 401), Enter-it -> code + name -> joined:
live owner output arrives (0 -> 20 `GH` lines in 12 s), `[data-terminal-recording]` present (REC chip visible to the guest). Screenshot: xplat-manual-X-23-20261113T1800Z-rx23c-safari-guest-rec-mac.png.
Cleaned: machine record revoked by exact id, pb uninstalled on macpool-7, macpool-7 released, private window closed.

## Still open on X-23
- pwsh 7 / PowerShell 5.1 multi-line paste on a Windows target runs the first lines before Enter: upstream PSReadLine limitation (section 1). Decision needed: Shift+Enter for newlines of a multi-line paste to a Windows target, or reword the row text "bracketed paste (PSReadLine on W...)".
- Physical keyboards: every key was driven by the OS (System Events, keybd_event) but no hand-typed hardware keyboard.
- Real Windows PC (victus) IME: victus has a live user session (rdp-tcp#1); no keys were injected into it, the IME check ran on a hosted runner's desktop.

## Cleanup / side effects on the pbmac VM
Enabled the Japanese Romaji input source (HIToolbox plist backup restored at the end), allowed pb's "local network" prompt (a leftover prompt of the fleet pb that blocked key events) and cancelled a "Keychain Not Found" prompt of the fleet pb: both were pending dialogs of the fleet's pb on the VM, not caused by this run.

Windows runner winpool-1 (own, started for the IME check) stopped; Edge profile and language packs went with it.
