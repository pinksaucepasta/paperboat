# X-23 third pass by hand, 2026-11-13 (IST ~19:00-23:00), live console, fleet account

Console: live at 9cdace229 (contains 657befa7e, the Option-key fix below). Safari 26.6.2 on macOS 26.6.2 (the pbmac Tart VM on apple), driven by safaridriver
over an ssh tunnel (key events go through WebKit's real keyboard path; Safari shows "remotely controlled by an automated test"), every run under
`flock fleet-mac.lock`, Safari and safaridriver quit after each run. Clipboard: the VM's real pasteboard (`pbcopy`) and a real Cmd+V.
Targets (both `pb version` dev-f61b6714d, today's main at 22:15 IST): **rx23-l** Linux arm64 (a throwaway OS user on oracle, personal) and
**rx23-w** Windows Server 2025 (winpool-1, team-owned machine of rx23-team with terminal+exec granted). Owner browser for the guest run: Chromium 1248 on oracle.
The pbmac VM is only the browser host here (no pb of its own is part of the check; its pb is the fleet's dev-79203d6e5).
Driver scripts were scratch (not committed): wd.py (W3C WebDriver client), st.py, lin.py, win.py, extra.py, guest.py, owner.mjs.

## Safari (macOS) -> Linux target rx23-l (bash), dev-f61b6714d, run 16:33Z
```
hold start 16:33:42Z
pw typed 28
me 200 https://oracle.newt-mermaid.ts.net/
PASS attach attached|
PASS shell starts 
PASS typed é 日 😀 echoed back wide ['Té日😀2']
--- cat -v: []
PASS Option+E then e (acute, no ESC) ['DK´e2']
PASS paste LF bracketed, each once early=0 got=[1, 1, 1]
PASS paste CRLF bracketed, each once early=0 got=[1, 1, 1]
PASS resize reaches PTY ['32 91']->['32 67']
selection set by script
PASS Cmd+C with a selection copies it and sends nothing to the shell selected=True no_^C=True clipboard_has_selection=True
PASS Ctrl-C without a selection interrupts 
PASS exit code 
PASS reload reattach attached|
PASS replay after reload 
PASS typing works after reload 
PASS tab hidden 130 s: still attached after coming back attached|
PASS tab hidden 130 s: typing works again 
PASS tab hidden 130 s: nothing lost (marker still on screen) 
```
Not shown as PASS lines: `cat -v` keys on this target (first run, dev-d77a40713): ^[[A ^[[B ^[[D ^[[C ^[[H ^[[F ^[[5~ ^[[3~ ^[OP ^[OQ ^[[15~ ^[[24~ for Up Down Left Right Home End PgUp Del F1 F2 F5 F12.

## Safari (macOS) -> Windows target rx23-w (cmd.exe, then PowerShell 5.1), dev-f61b6714d, run 16:40Z
```
hold start 16:40:35Z
pw typed 28
me 200 https://oracle.newt-mermaid.ts.net/
PASS attach attached|
default shell banner: ['Microsoft Windows [Version 10.0.26100.33674]', '(c) Microsoft Corporation. All rights reserved.', 'C:\\Users\\runneradmin>echo CM$((1+1))X']
PASS start powershell 
PASS shell starts 
PASS utf8 wide render 
typed utf8: True
--- keys: 'K:UpArrow|0|0 K:DownArrow|0|0 K:LeftArrow|0|0 K:RightArrow|0|0 K:Home|0|0 K:End|0|0 K:PageUp|0|0 K:Delete|0|0 K:F1|0|0 K:F2|0|0 K:F5|0|0 K:F12|0|0 K:B|Alt|98 K:F|Alt|102 K:U|Control|21 K:Backspace|Alt|8'
PASS keys arrive: arrows, Home, End, PgUp, Del, F1 F2 F5 F12, Option+Left/Right as Alt+b/f, Cmd+Backspace as Ctrl+U, Option+Backspace 
PASS paste LF: every line runs once, in order, not doubled got=[1, 1, 1] order=[35, 37, 39] run_before_Enter=2
PASS paste CRLF: every line runs once, in order, not doubled got=[1, 1, 1] order=[36, 38, 40] run_before_Enter=2
PASS exit code 
PASS resize reaches PTY ['89']->['66']
PASS REC chip appears 
PASS REC chip still there after typing 5 s later 
PASS reload reattach attached|
PASS replay after reload 
PASS Ctrl-C interrupts 
hold end 16:42:34Z
```
The Windows paste lines: PowerShell 5.1's PSReadLine 2.0 has no bracketed paste, so the first two lines run when pasted (counted as run_before_Enter=2); every line runs once, in order, not doubled.

## Guest by code, no account (cookies cleared, /api/v1/me 401), Safari on macOS -> team Windows target rx23-w, owner Chromium (Linux), run 16:44Z
```
hold start 16:44:17Z
pw typed 28
me 200 https://oracle.newt-mermaid.ts.net/
PASS attach linux attached|
ime: dispatched
PASS IME composition commits once (synthetic composition events) ['rx23u@oracle:~$ echo IME日本語', 'IME日本語']
PASS attach windows attached|
PASS pwsh 7 starts 
FAIL pwsh 7 bracketed paste: nothing runs before Enter, each line once run_before_Enter=2 got=[1, 1, 1]
me after clearing: 401
PASS guest sees live owner output '                                                                               \n'
PASS output keeps arriving 14->14
PASS REC indicator visible to guest 
PASS guest cannot type (watching only) see screenshot
hold end 16:45:52Z
```
Screenshot: xplat-manual-X-23-20261113T1700Z-rx23b-safari-guest-rec.png (REC chip, "Watching only", 2 people, owner's live output GH118..GH128).

## REC on a Windows target (the 0.4 s bug)
Console Start recording on rx23-w (team terminal, owner Chromium on Linux and Safari on Mac): REC chip appears, stays 6 s after typing three lines, Stop ends it; server records on rx23-w
last 10-11 s (rec_68f9dff1.., rec_3e1f1ac9.., rec_9a0b193b.. in `pb recordings list --workspace rx23-team`), not 0 s as rec_2eb79015 on 2026-11-13 04:06 UTC.
Cause of the old 0 s records: the Start button kept keyboard focus, so the Enter typed into the shell toggled recording (fixed in ec187063c, on the live console since 15:01 IST; the host fix a2a0b2b1a ends a deleted session's records).

## Product bugs found and fixed in this pass
| Bug | Commit | Test |
|---|---|---|
| Safari: Option+Left/Right sent ESC+U+001C/U+001D (Safari reports key U+001C with code ArrowLeft; `cat -v` showed ^\ and "Quit (core dumped)"), Option+E sent ESC+acute (a broken byte in bash) | 657befa7e | console/lib/terminal/mobile-keys.test.ts |
| Windows target: Ctrl-C ended a PowerShell started from cmd.exe (Start-Sleep 300 + Ctrl-C returned to cmd) because the 100 ms ETX fallback terminates the program cmd waits on | f61b6714d | TestHandlesInterruptItself (ran, cli CI run 43455231297); TestNativeConPTYInterruptKeepsAPowerShellStartedFromCmd (Windows ConPTY, cross-platform workflow); device: Chromium -> rx23-w on dev-f61b6714d, PS stays after Ctrl-C |

## Not done or open (named)
- Real macOS IME (Pinyin/Kotoeri): only synthetic composition events dispatched in Safari (commit once: PASS) and CDP composition in Chromium (earlier pass). Real IME from Edge/Chrome on Windows: not run.
- Safari's own Cmd-+ zoom: Safari refuses UI interaction while automated (modal "remotely controlled"), so 200% zoom in Safari was not run (Chrome on macOS passed it in the first pass, 2026-11-13 after server .16).
- Physical keyboards: all keys were driven (WebDriver, CDP).
- REC chip for a guest on a **macOS target**: no team-owned Mac target with recording (macpool queue); REC seen for guests on Linux and Windows targets.
- PowerShell 7 (pwsh 7.6.9, PSReadLine 2.4.5) bracketed paste to a Windows target: see the last section.

## PowerShell 7 bracketed paste to a Windows target (open, cause not found)
Safari -> rx23-w, `pwsh -NoLogo`, real Cmd+V of three lines: the first two lines run at once, the third waits for Enter (run_before_Enter=2, each line once, in order), also after
`Set-PSReadLineOption -BracketedPasteEnabled $true` (hold 16:55Z). The same paste to bash on the Linux target arrives bracketed (0 run before Enter). The wrap is decided by @wterm/dom
(input.js:430, only when the ghostty core saw ESC[?2004h); a raw `pb connect rx23-w` capture of the pwsh session contains ESC[?2004h and ESC[?2004l, but `pb connect` itself enables
the mode on attach, so that does not prove pwsh's own sequence reached the browser. Not narrowed further (budget): next checks are Safari/Chrome WebSocket frames for ESC[?2004h inbound and ESC[200~ outbound.
