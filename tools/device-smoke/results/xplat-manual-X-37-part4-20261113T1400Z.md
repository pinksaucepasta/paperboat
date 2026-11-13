# X-37 part 4 by hand, UTC 2026-11-13 13:00 - 13:35 (IST 18:30 - 19:05 on the oracle clock; machine clocks UTC)

Build under test: main d77a40713 (`pb version` = dev-d77a40713) on every machine used:
- xr37-l (Linux container on hp), xr37-l2 (second Linux container on hp, same throwaway account, bridge = direct path), xr37-m2 (macOS arm64 GitHub runner, macpool-5), xr37-w (Windows GitHub runner, winpool-3).
- Files generated on the machine from /dev/urandom or a counter-stamped 1 MiB block, sha256 compared on both ends; nothing big crosses ssh.

## 1. Unlimited send, UDP blocked on the sender then returned (L>L2, 2.5 GB, direct baseline)
```
E1 (iptables drop of every non-DNS UDP on the sender after 20 s, removed 90 s later, no restart):
   "Waiting for a direct connection or your relay: only a Paperboat relay reaches this machine now" printed, receiver bytes frozen for the whole block;
   bytes flowing again within 10 s of removing the block; delivered, rc 0 (e1.bin ... delivered to /home/pbt/Paperboat/Inbox).   PASS
```
So the earlier "stays on the relay after UDP returns" on W>L and W>M (parts 2 and 3) is not a general resume bug: with a direct baseline a Linux sender is back in 10 s on main (8f95a82b9 wake fix included). Those W cases are runners that never had a direct path to the other machine.

## 2. Sender daemon restart during an Unlimited send (L>L2, 2.5 GB)
```
E2 direct path, `pb service restart` on the sender 20 s in: send CLI printed the waiting line while the daemon was away, then continued; delivered, sha256 equal (86835477f2e059e0 both ends).   PASS
E3 UDP blocked at +20 s, sender daemon restarted at +42 s, block removed at +62 s: receiver bytes frozen at 111 MB for 4+ minutes after the block was removed,
   the CLI ended "Paperboat could not finish the command" (support_204e470f-f312-4c78-b0b1-5484ec8c2e0e).   FAIL (not delivered)
E4 same steps (block, restart 25 s later, unblock 20 s after that): bytes flowing again 10 s after the block was removed (221 MB -> 434 MB -> 666 MB ... 2.5 GB at ~25 MB/s, before any pb ping).
   The run was not completed: hp reached load average 216 (other lanes' jobs), xr37-l2 went offline in the account list and the sender showed "Waiting for xr37-l2 to come back online".   NOT A RESULT for delivery
```
E3 and E4 are the same steps with different outcomes, and hp was at load 216 during both, so the freeze after "restart while blocked" is intermittent and may be load. Not reproduced twice; not fixed. Read-only code reading (Haiku, not confirmed on a device): after a restart the engine's first STUN runs while UDP is blocked and the advertisement is not repeated without traffic (mesh/engine_backend.go onEngineStatus, regional_status.go ReSTUN has one caller); candidate fix is to ReSTUN from WakePeer. Touches the shared peer engine: not changed on launch eve.

## 3. Windows sender, Unlimited 4.7 GB (4,718,592,000 B) to the macOS receiver (W>M2)
```
13:13:46 start; 13:13:52 "Waiting for a direct connection or your relay: this machine is not reachable now"; 13:13:54 "...only a Paperboat relay reaches this machine now"
pb ping from W to the Mac says regional_relay (4/4 samples) while pb ping from the Mac to W says direct (27 ms) - the same asymmetry as part 3.
Still waiting after 10 minutes when the runner's ssh session (and the winpool-3 runner) ended. No bytes sent.   NOT COMPLETED (relay-only path: the waiting state is the designed result; no direct path exists from this Windows runner)
```

## 4. Not run (named)
- Automatic 2,000,000,000 B L>M (hosted macOS relay about 0.27 MB/s, 2 GB = 2 h; the part-3 attempt failed at 892 s).
- Unlimited above 4 GiB with Linux or macOS as the direct sender/receiver through a path that is direct: hosted Mac and Windows runners only reach the hp container through the hosted relay, which Unlimited refuses by design; own relay (pbh) not set up.
- Sender or receiver sleeping (hosted runners and the Linux containers cannot sleep).
- macOS FAT32/exFAT receiver and APFS or ext4 receiver above 4 GiB.

## 5. Cleanup
xr37-l2 revoked by exact id (machine_aeea7cd8-c09f-459f-8509-7a3c874cc475) and its container removed; macpool-5: `pb uninstall`, claim released; xr37-l container and big files removed from hp; winpool-3 runner had already ended.
