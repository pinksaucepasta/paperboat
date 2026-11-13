# X-37 part 6 by hand, UTC 2026-11-13 19:20 - 21:40 (IST 00:50 - 03:10), lane R-X-37D

Build under test: dev-e4515714f (`pb version` on all three) on the shared fleet: lanex2-l2 (Linux arm64 container on anvit), lanex2-m (macOS, pbmac VM), lanex2-w (the real Windows 11 PC victus). `pb ping` in all six directions at 19:2xZ: path=direct. Files are generated on the sender (openssl AES-CTR stream on Linux, random 1 MB block with a counter on Windows), sha256 compared on both ends; every transfer is `pb send <file> --to <machine> --unlimited`, 4,500,000,000 bytes.

## 1. Unlimited above 4 GiB, all four directions the earlier lanes could not get direct (all PASS, sha256 equal, files and Inbox copies removed afterwards)
```
L>W  19:22:04Z - 19:48:33Z (26.5 min, ~2.9 MB/s)  rc 0, delivered to C:\Users\Pujan\Paperboat\Inbox, sha256 acc4f7c9a3e60626... both ends
W>L  19:54:50Z - 20:22:17Z (27.4 min)             rc 0, delivered to /home/tester/Paperboat/Inbox, sha256 2412fea7a6c84b01... both ends
W>M  20:09:35Z - 20:16:56Z (7.3 min)              rc 0, delivered to /Users/admin/Paperboat/Inbox (APFS), sha256 2412fea7a6c84b01... both ends
L>M  20:16:59Z - 20:41:59Z (25 min)               rc 0, delivered to /Users/admin/Paperboat/Inbox (APFS), sha256 acc4f7c9a3e60626... both ends
```
Path stayed direct (ping direct before and after). The waiting line "Waiting for a direct connection or your relay: this machine is not reachable now" printed once for 1-2 s at the start of each send to the Mac and went away by itself.

## 2. Coordinator's question: does a Windows>Linux Unlimited send only start after a Windows daemon restart (R-X-37C part 5)?
No. W>L above started by itself and ran at ~3 MB/s (committed offset 333 MB after 2 min, 1.50 GB at 9 min, 3.38 GB at 19 min), with no daemon restart, with the path direct both ways. In part 5 the Windows hosted runner had `regional_relay` towards the Linux machine while the Linux machine saw it direct; on victus the path is direct in both directions, so the stall does not repeat.
(Harness note: a send started with Start-Process over ssh dies when the ssh session ends on Windows; run `pb send` inside the ssh session instead. Not a product bug.)

## 3. macOS receiver that cannot store the file
```
Inbox on a 6 GB FAT32 (msdos) sparse image, `pb inbox set /Volumes/RX37DFAT/Inbox` accepted; L>M 4,500,000,000 B Unlimited:
pb: lanex2-m keeps its Inbox on a FAT volume, which cannot hold a file over 4096.0 MiB (this one is 4291.5 MiB). Send a smaller file, or point its Inbox at a disk with NTFS, exFAT, APFS or ext4 with `pb inbox set`. Nothing was sent.
Inbox folder empty afterwards (no partial file, no .publish leftover).  PASS (the receiver names the cause before a byte is sent, so no OS write error is produced)
```
exFAT receiver above 4 GiB on macOS (6 GB exFAT sparse image, W>M 4.5 GB): NOT COMPLETED. exFAT has no hard links, so the receiver copies the spooled file into the Inbox (`.transfer_<id>.publish`, 1.2 GB at 9 min, 2.75 GB at 21 min, then 2.68 GB at 36 min: the copy restarted) at 2-6 MB/s through macOS's user-space exFAT driver, while the sender's record already says `published`. I cancelled it (the Mac had been busy 36 min and others were queued): the image was force-detached, the Inbox set back to /Users/admin/Paperboat/Inbox, nothing left. `pb send cancel` of that transfer answers "could not finish the command" (state published).

## 4. `pb inbox path` on an Inbox the OS refuses (fixed, c1435ee54)
Before (fleet dev-e4515714f, Windows Inbox directory with a deny ACL): `pb inbox path` rc 1, "Paperboat could not finish the command". Now it names the folder and the OS error and says `pb inbox set <folder>`; tests TestInboxPathNamesTheFolderAndTheCauseInsteadOfCouldNotFinish and TestValidatePathNamesTheOSCauseOfAnInboxItCannotWrite (CI cli run 43491205179, both PASS in the log).

## 5. Windows receiver on a write-protected volume (new finding, fixed in 27b9e9c25)
Probe on victus: an NTFS VHDX attached read-only (diskpart `attach vdisk readonly`) as R:, Inbox R:\Inbox: `EnsurePath` returns `chmod R:\Inbox: The media is write protected.` (ERROR_WRITE_PROTECT, 19), which was not in the receiver's permanent-failure list (Windows has no EROFS and 19 is not fs.ErrPermission), so a send was answered 503 and retried until the timeout with no cause (a deny-ACL Inbox ended `storage_unavailable` the same way: result_code storage_unavailable, sender silent for 5 min; the Inbox's ACL is reset to owner-only by EnsurePath, so a deny ACL is not a valid read-only test on Windows).
Fix: windows-only writeProtected() feeds permanentStorageFailure: 507, retryable false, details.os_error "The media is write protected.". Test TestWriteProtectedWindowsVolumeIsAPermanentInboxRefusal PASS on victus (go test -c for windows built on anvit, run on victus) and the existing TestInboxWriteRefusedByTheOperatingSystemIsPermanentAndNamesTheCause PASS there too; linux caller tests: ci-test cli run 43509914206 green. Not run end to end: a real send into the read-only VHDX on a build with the fix (the fleet still carries dev-e4515714f and only T-FLEET2 installs).

## 6. Not done, named
- exFAT receiver above 4 GiB on macOS (section 3: very slow copy fallback, restarted once), end-to-end Windows write-protected receiver on a build with 27b9e9c25 (section 5).
- Unlimited M as sender above 4 GiB on dev-e4515714f (last seen M>W direct 4.5 GB on earlier builds; no more multi-GB Mac sends while others wait for the Mac).
- Sender or receiver sleeping: X-26. Linux sender restarted while UDP is blocked froze once under load (part 4 E3): not re-run (no network cuts on the fleet).
