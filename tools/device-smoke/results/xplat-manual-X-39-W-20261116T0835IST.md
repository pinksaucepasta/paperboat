# X-39 W: victus (real Windows PC) reboot with nobody signed in, reachable from L and M (lane FLEET4), 2026-11-16 08:34-08:40 IST

Builds: lanex2-w (victus, Windows 11) and lanex2-l2 on dev-a2742604e (today's main); lanex2-m on dev-432169341 (= a2742604e + an Apple_Terminal-only change). Observers: lanex2-l2 and lanex2-m, `pb exec lanex2-w -- hostname` every ~15-50 s. One `shutdown /r /t 5 /f` issued 08:35:20 IST (inside the announced window, under fleet-power.lock and fleet-mac.lock).
Result PASS. Timeline: exec from L and from M fails at +33 s and +69 s (machine down/booting: "Paperboat could not reach the target machine"), then succeeds from both at +120 s (08:37:20), and at +175 s and +225 s: three double successes. Sessions at the first success: only `services` and `console` (state Conn, no user name), no user logged in; Security log type 2/10/11 logons since the boot are only DWM-1/UMFD-0/UMFD-1 (system), no person; kernel boot 08:36:01. All six Paperboat services Running with nobody logging in; pb version dev-a2742604e; `pb doctor` rc 0. Hyper-V VMs came back as before (pbvm2 and pbvm4 Running by auto-start, pbvm3 Saved, others Off).
Script note: its "ssh answers again 10 s after the command" line is wrong (victus was still shutting down at that poll); the observer timeline above is the evidence. The baseline sessions print was empty (ssh quoting); the boot time before was 2026-11-14 06:30:38.
Not produced: Windows fast startup (a fast-startup shutdown needs a shutdown of victus, which the user forbids: it must stay on; hibernation is proven by R-X-26 40e072829 and a restart bypasses fast startup).

## Run log
```
08:34:40 before: W 00fa6e5005b","version":"dev-a2742604e"}}
08:34:58 baseline L>W rc=0 Victus
08:35:19 baseline M>W rc=0 Victus
08:35:20 baseline sessions/boot/vms:

08:35:20 shutdown /r issued (one reboot)
08:35:30 victus ssh answers again 10 s after the command
08:35:31 sessions at ssh-back:
SESSIONNAME               USERNAME                 ID  STATE   TYPE        DEVICE 
>services                                            0  Disc                        
                                                     1  Down                        
                                                     2  Down                        
 rdp-tcp                                         65536  Listen                      

Saturday, November 14, 2026 06:30:38 AM
Monday, November 16, 2026 08:35:31 AM
08:35:53 t+33s L>W rc=1 pb: Paperboat could not reach the target machine.  | M>W rc=1 pb: Paperboat could not reach the target machine. 
08:36:30 t+69s L>W rc=1 pb: Paperboat could not reach the target machine.  | M>W rc=1 pb: Paperboat could not reach the target machine. 
08:37:20 t+120s L>W rc=0 Victus | M>W rc=0 Victus
08:37:21 sessions at first double success:
SESSIONNAME               USERNAME                 ID  STATE   TYPE        DEVICE 
>services                                            0  Disc                        
 console                                             1  Conn                        
 rdp-tcp                                         65536  Listen
08:38:15 t+175s L>W rc=0 Victus | M>W rc=0 Victus
08:39:05 t+225s L>W rc=0 Victus | M>W rc=0 Victus
08:39:05 first L+M exec success 120 s after the command; good=3
08:39:06 final sessions/services/vms:

08:39:10 pb version W after: 00fa6e5005b","version":"dev-a2742604e"}} doctor rc=0
08:39:10 RESULT PASS
x39rc=0
```
