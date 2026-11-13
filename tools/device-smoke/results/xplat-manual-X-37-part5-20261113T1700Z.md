# X-37 part 5 by hand, UTC 2026-11-13 15:05 - 17:00 (IST 20:35 - 22:30)

Build under test: dev-72c817bfc (main 72c817bfc; origin/main now 982827507, later commits are X-38 docs and staging only) on every machine used:
- xr37c-l, xr37c-l2: Linux containers (Ubuntu, arm64, privileged, systemd) on anvit, same docker bridge, throwaway account.
- xr37c-w: Windows GitHub-hosted runner (winpool-4, Windows Server, NTFS). `pb version` = dev-72c817bfc on all three.
- xr37c-m: macOS arm64 GitHub runner (macpool-8, macOS 26.7.2), requested 15:08Z, up at 16:55Z (GitHub macOS queue 1 h 47 min), dev-72c817bfc, later dev-41bd5db69 for section 8. winpool-4 ended at 18:10Z (3 h runner limit).
- Files are generated on the machine (openssl AES-CTR stream on Linux, 1 MiB random block with an 8-byte counter on Windows), sha256 compared on both ends; nothing big crosses ssh.

## 1. Unlimited above 4 GiB, Linux sender, Linux receiver, direct (L>L2, 4,500,000,000 B)
```
pb ping L>L2: direct (bridge). pb send lw45.bin --to xr37c-l2 --unlimited
Run A, receiver container paused (docker pause = process freeze, like a sleeping machine) for 120 s at 1.0 GB: the sender printed "Waiting for xr37c-l2 to come back online (up to 32 min)" and "Waiting for a direct connection or your relay: only a Paperboat relay reaches this machine now";
   about 40 s after the unpause bytes flowed again from the committed offset (1118 MB -> 1446 MB), delivered after 16 min, sha256 equal (e92e02426e0d5817 both ends), ext4 (overlayfs on the host's ext4) receiver. PASS
Run B, SENDER container paused for 120 s at 1.0 GB: after the unpause the spool stayed at 5334 MB for ~2.5 min (sender CLI printed "Waiting for a direct connection or your relay: this machine is not reachable now"), then continued from the committed offset, delivered, sha256 equal (e92e02426e0d5817), 26 min in all. PASS (resume is slow: 2.5 min)
```
## 2. Unlimited above 4 GiB, Windows sender, Linux receiver (W>L, 4,500,488,192 B)
```
Before the Windows daemon restart: W `pb ping xr37c-l` = regional_relay (4/4, also pb doctor "xr37c-l relay") while L `pb ping xr37c-w` = direct; the send printed "Waiting for a direct connection or your relay: only a Paperboat relay reaches this machine now" for 8 minutes.
`pb service restart` on W: W>L direct at once and the waiting send started moving by itself (about 1 MB/s at first, 1.3 GB at the time I looked, ~65 MB/min later);
delivered 16:37Z, sha256 equal (c387c06e9c0ccc97 both ends), Linux Inbox. PASS (Windows as sender above 4 GiB, Linux as receiver above 4 GiB)
```
## 3. Linux sender, Windows receiver, Unlimited above 4 GiB (L>W): NOT COMPLETED
```
L>W was relay-only for the whole hour: `pb ping xr37c-w` on L = regional_relay (also after `pb service restart` on L and again on W), W>L = direct. Direction follows who initiates: the side behind the hosted runner's NAT (W) finds the direct path to the Linux machine, the Linux machine never finds one to W. Unlimited refuses relays: "Waiting for a direct connection or your relay: only a Paperboat relay reaches this machine now". At 15:20Z, before any restart, L>W said direct once (4/4).
```
Two earlier "W relay asymmetry" cases (W>M parts 3 and 4) are the same thing, not a general resume bug. 60e2e0ad9 (ReSTUN on wake) does not change it: it only re-learns the sender's own endpoints on a transfer retry.

## 4. Verify W>M direct path (brief item)
```
17:00Z W `pb ping xr37c-m`: regional_relay (and 1 of 3 lost); M `pb ping xr37c-w`: direct 31 ms once at 16:56Z, regional_relay at 17:20Z and 18:08Z; L>M and M>L: regional_relay.
18:08Z W>M `pb send --unlimited` of 4.5 GB: "Waiting for a direct connection or your relay: this machine is not reachable now", with M pinging W every 10 s (relay each time); the Windows runner ended at 18:10Z, nothing sent.  NOT COMPLETED
```
W>M is not direct on dev-72c817bfc: 60e2e0ad9 did not change it. Two hosted runners (Azure Windows, GitHub macOS) sit behind NATs that never produce a direct path towards each other except the one lucky moment.

## 5. Automatic 2,000,000,000 B (sha256 bf512edf575cb294 both ends)
```
L>W  PASS rc 0, 32 min (16:47-17:19Z), delivered to C:\Users\runneradmin\Paperboat\Inbox, sha256 equal, `pb ping` L>W direct at the end (about 1 MB/s)
L>M  PASS rc 0, 64.5 min (16:59-18:04Z), macOS Inbox, sha256 equal, path regional_relay (hosted relay, 0.5 MB/s)
```
(the part-3 L>W and L>M failures were the fleet container's 1 GiB memory limit and an abandoned run.)

## 6. Receiver cannot store the file: error with the cause (new, X-37 row text of 22:30 IST)
```
dev-72c817bfc, Linux receiver Inbox on a read-only ext4 loop mount, 1 MB Automatic send: ended "Paperboat could not finish the command" (no cause): the receiver answered storage_unavailable as HTTP 503, which the sender retries until its timeout.  FAIL
Fix bfd990648 (tests TestInboxWriteRefusedByTheOperatingSystemIsPermanentAndNamesTheCause, TestStorageFailureCarriesTheReceiversOSErrorAndIsNotRetried, TestSendToAnInboxTheReceiverCannotWriteNamesTheOSError; ci-test cli green): EROFS / access denied / name too long / file too big answer 507 with the OS error text and are not retried.
dev-41bd5db69 (that fix on main 982827507): Linux ro Inbox: "xr37c-l2 could not store the file in its Inbox: read-only file system. Nothing was delivered. Fix that disk or point its Inbox at another one with `pb inbox set`..." (JSON destination_cannot_store, retryable false) PASS
dev-41bd5db69: macOS exFAT image attached read-only as the Inbox (hdiutil attach -readonly), send from Linux: the same message, "read-only file system" PASS
dev-41bd5db69: Linux FAT32 Inbox (12 GB vfat loop mount, mounted in the runtime's mount namespaces), 4.4 GB Unlimited send: "keeps its Inbox on a FAT volume, which cannot hold a file over 4096.0 MiB (this one is 4196.2 MiB) ... Nothing was sent." PASS
macOS `pb inbox set` accepts a FAT32 (msdos) and an exFAT image as the Inbox with no message (Windows refuses FAT/exFAT for privacy); a >4 GiB send to a Mac FAT32 Inbox needs a direct sender: not run.
```
Seen, not fixed: `pb inbox path` on a receiver whose Inbox is read-only ends "Paperboat could not finish the command" (inbox.ValidatePath error not shown).

## 7. Not run, named
- Unlimited above 4 GiB with a Mac as receiver or sender to a non-Windows peer, macOS FAT32/exFAT/APFS receivers above 4 GiB: no sender had a direct path to the hosted Mac (section 4); own relay (pbh) not set up.
- Unlimited L>W above 4 GiB (section 3) and W>M (section 4): hosted-runner NAT.
- Sender or receiver sleeping on macOS and Windows (hosted runners cannot sleep; Linux covered by docker pause only).
- Linux sender restart while UDP is blocked froze once in part 4 (E3), not re-run.
