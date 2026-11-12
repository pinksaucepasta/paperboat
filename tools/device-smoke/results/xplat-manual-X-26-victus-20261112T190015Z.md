# X-26 sleep and wake by hand on victus (real Windows 11 machine), 20261112T190015Z (UTC)

victus: pb dev-7cf73f208, Modern Standby (powercfg /a: Standby S0 Low Power Idle, Network Connected; S1-S3 not available), account observer = another machine of the same account (dadape, Linux) running `pb machine list --q relmw-victus`.

## Steps and what was seen
1. 18:33:12 UTC: a wake timer was registered as a SYSTEM scheduled task (-WakeToRun, once at 18:37:11 UTC); `powercfg /waketimers` listed it ("Windows will execute 'NT TASK\xpcwake' scheduled task that requested waking the computer"). A client `pb ssh dadape -- <counter loop>` was started by a scheduled task as the user (it wrote no output, see below).
2. 18:33:32 UTC: sleep requested through a SYSTEM task running `rundll32.exe powrprof.dll,SetSuspendState 0,1,0`; the System log confirms "Sleep Time 2026-11-12T18:33:32Z".
3. TARGET ASLEEP, seen from the account's other machine: relmw-victus was `online` until 18:33:59, then `offline` at 18:34:50 UTC (78 s after the sleep started). The machine list carried no offline reason field (state offline only). PASS: a sleeping target shows offline.
4. 18:37:11 UTC (wake timer due): victus did NOT wake. ssh (tailscale) timed out until 18:57. FAIL: Windows Modern Standby ignored the scheduled wake timer (the task never ran: C:\Users\Pujan\xpc-wake.txt was never written; `powercfg /lastwake` shows no wake source).
5. The user woke it with a key press: System log (Power-Troubleshooter) "returned from a low power state, Sleep Time 18:33:32Z, Wake Time 18:57:08Z"; boot time unchanged (16:16:38 UTC, so a resume, not a reboot). pbvm2 and pbvm4 Running throughout the check (no restart needed).
6. ONLINE AGAIN: the observer machine saw relmw-victus `online` again at its next check, 18:59:19 UTC (within 2 min 11 s of the wake; the poller that would have given the exact second had ended at 18:40). The ssh to victus answered at 18:58.
7. Client session across the sleep: NOT TESTED. The scheduled-task counter (`pb ssh dadape` from victus) produced no output (0 lines), so there was no session to resume; terminal, transfer, tunnel and preview resume across a sleep were not run.

## Other OSes
Linux: a container cannot sleep (n/a). macOS: `sudo pmset sleepnow` on the pbmac VM answers "Unable to sleep system: error 0xe00002e2" (the VM cannot sleep); a real Mac is needed.
