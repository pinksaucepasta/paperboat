# X-26 L↔M, the Linux-asleep direction with the Mac as the observing peer (lane FLEET4, 2026-11-16 23:42-23:50 IST = 18:12-18:20Z)

Build dev-6232eebec on both machines (installed 23:38-23:40 IST; contains the FIX-REFRESH CLI fixes d189bcf31/8d20fe14f, SSH CA 5177c7857, 6302166fc, guard Rev 2 sources): lanex2-l2 (Linux container on anvit, SLEEPER) and lanex2-m (the pbmac Tart VM, peer, fleet login session_147fc4ce). The other direction (Mac asleep, Linux peer) is results/xplat-manual-X-26-M-FLEET4-clean-20261116.md. Driver of R-X-26-N (FLEET=1 PEERS=m): FROZEN 18:12:53Z = SIGSTOP of all 12 pb processes of the container + iptables owner DROP of uid 1001 (tester) for 240 s, thaw ~18:16:53Z; systemd WatchdogSec=0 drop-ins in /run for the paperboat units during the run (SIGSTOP trips the 30 s watchdog; removed afterwards, units restarted); under fleet-mac.lock with a 5 min cooldown.

## Processes
- lanex2-l2 at the audit (after the run): daemon, hostd, worker, updated, 3 session owner/carrier pins: sha256 prefix de4228e4 = dev-6232eebec's pb-linux-arm64 (de4228e435f77df447242064c94bc80adf4c0456dfb84a08d125be2c2ef22c31); the machine guard (/usr/local/libexec/paperboat-machineguard/pb) was still dev-d57a6af85 (the explicit guard install follows this run), and 3 test `pb connect` clients of my own earlier X-26 runs (dev-d57a6af85, killed by pid afterwards). 
- lanex2-m: user daemon, hostd, worker, carrier, host-service, session owner, updated = d529d960 (dev-6232eebec); the previous generation's session owner (8d52ce65 = dev-d57a6af85, which already holds the FIX-REFRESH fixes) and the old machine guard (d57a6af85) still run.
So no process predating the FIX-REFRESH fixes was running.

## Run result: allpass True
```
18:12:41 RESULT PASS terminal attach+type before: inm 0.5s
18:12:41 RESULT PASS terminal attach+type before: outm 0.0s
18:12:53 FROZEN at 18:12:53 frozen 12 pids; rule: 1 | 
18:16:11 RESULT PASS offline seen from m after 83s: lanex2-l2 personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_mana
18:16:11 RESULT PASS offline reason from m lanex2-l2 personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_mana
18:16:11 RESULT PASS exec to frozen refused clearly from m (124, 'timeout')
18:19:55 RESULT PASS recovers: tunnel serves 0.6s after thaw
18:19:55 RESULT PASS recovers: exec S->m 3.0s after thaw
18:19:55 RESULT PASS recovers: preview serves 3.5s after thaw
18:19:55 RESULT PASS recovers: exec m->S 3.7s after thaw
18:19:55 RESULT PASS recovers: term outm 3.9s after thaw
18:19:55 RESULT PASS recovers: term inm 4.9s after thaw
18:19:55 RESULT PASS recovers: list online from m 5.5s after thaw
18:19:55 RESULT PASS terminal inm: typed-while-frozen ran once count=1
18:19:55 RESULT PASS terminal outm: typed-while-frozen ran once count=1
18:20:06 RESULT PASS terminal reattach to the held session after wake replay has pre marker=True live after 0.5s
18:20:06 RESULT PASS send in flight P1->S send (S receiver frozen) rc=0 inflight_at_freeze=(True, True) ommitted_offset":60000000,"state":"published","result_code":"published","created_at":"2026-11-16
18:20:06 RESULT PASS send in flight S->P2 send (S sender frozen) rc=0 inflight_at_freeze=(True, True) 44","committed_offset":60000000,"state":"published","result_code":"published","created_at":"2026-1
18:20:09 RESULT PASS fresh send m->S after wake {"data":{"batch_id":"batch_8e3b5620-4be6-46ec-b5de-4a0d31b4d55b","transfers":[{"transfer_id":"transfer_9cbc7e9c-cd4d-4634-b3b1-83d5dd227f8e","batch_id":
18:20:14 allpass True
```
