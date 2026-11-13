# X-14 (receiver-name refusal) and X-25 (Windows client, alternate screen) by hand on the shared fleet, 2026-11-13 ~12:00-12:20 IST (lane F-FIN1)

Build under test: `pb version` on lanex2-l (Linux container on hp), lanex2-m (macOS, the pbmac VM) and lanex2-w (the real Victus Windows 11 PC): **dev-918afefe9** on all three (main 918afefe9; contains ab56fa6f9 and 8d78dbd88). Commands run through the fleet harness (`xplat_fleet.py` devices, ssh/docker exec/powershell), no secrets printed.

## X-14: `pb send 'sp ' --to lanex2-m`
| from | command (in the directory holding the file `sp `) | result |
|---|---|---|
| lanex2-l | `pb send 'sp ' --to lanex2-m` (also with the absolute path) | rc 2: `Sending to lanex2-m (machine_9e18...)` / `pb: Nothing was sent: lanex2-m cannot store "sp ": the name starts or ends with white space, which a Team Inbox transfer does not carry. Rename the file and send again.` Names the file and the reason. PASS |
| lanex2-w | file created with the extended-length path `\\?\C:\Users\Pujan\fin1-x14\sp ` (Get-ChildItem shows `[sp ]`); `pb send "\\?\C:\Users\Pujan\fin1-x14\sp " --to lanex2-m` (powershell and plain cmd) | rc 2: `pb: \\?\C:\Users\Pujan\fin1-x14\sp does not exist. Check the path, then retry pb send.` FAIL: the trailing space is lost before the name check. Cause: `filepath.Abs` (send_command.go) = GetFullPathName, which strips trailing spaces even from a `\\?\` path (checked on lanex2-w with a P/Invoke of GetFullPathNameW: `...\sp ` -> `...\sp`). Without the extended-length form the file cannot be named at all on Windows (the harness's `sp ` is created as `sp` and then delivered as `sp`). Fixed in ecae8c544 (sendSourcePath), waits for the next client build. |

## X-25: Windows client joins a Unix-target session with a full-height `less` open
Method: starter on the other Unix OS at 30x100 runs `pb connect <target> new --name fin1-less-...`, then `less` of a 300-line file whose first line is `ALT_42 TOP ROW`; the Windows client (ssh -tt to Victus, ConPTY, 30x100) runs `pb connect <target> --session <name>`; its raw output after the last `ESC[?1049h` is rendered through pyte (100x30) and row 0 is read. Sessions closed and deleted afterwards.
| target | starter | Windows client sees | result |
|---|---|---|---|
| lanex2-l | lanex2-m | row0 `ALT_42 TOP ROW`, row1 `ALT_42 line 001`, row2 `ALT_42 line 002`, last row `:` | PASS |
| lanex2-m | lanex2-l | row0 `ALT_42 TOP ROW`, row1 `ALT_42 line 001`, row2 `ALT_42 line 002`, last row `:` | PASS |
