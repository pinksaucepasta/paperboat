# X-37 part 3 by hand, UTC 2026-11-13 09:15 - 11:40 (IST 14:45 - 17:10)

Machines and builds (`pb version` of each):
- Fleet lanex2-l (Linux container on hp, 1 GiB memory limit), lanex2-m (macOS, apple VM), lanex2-w (real Windows 11 Victus), all dev-cf0c0a620. Used only for the four 2 GB sends in section 1 (coordinator ruling 14:50 IST: no big transfers to or from the fleet after the lanex2-l container was OOM-killed; leftovers removed).
- Own throwaway account (console sign-up), `xr37-l` = Linux container on hp (Ubuntu 24.04 amd64, privileged, 6.6 GiB), `xr37-m` = GitHub macOS runner (macpool-1), `xr37-w` = GitHub Windows runner (winpool-3). dev-cab625433 for sections 2-6, then dev-b239e6f32 (main with the fix of section 7) on all three, `pb exec` both ways between all three on it: 6/6 rc 0.
- Files are generated on the machines (1 MiB random block, 8-byte counter overwritten per block) and compared by sha256 on both ends; nothing big crosses ssh.

## 1. Automatic, 2,000,000,000 bytes (fleet, dev-cf0c0a620)
```
M>W  PASS rc 0, 415 s, sha256 equal, path direct
W>L  PASS rc 0, 845 s, sha256 equal, path direct
M>L  PASS rc 0, 859 s, sha256 equal, path regional_relay (hosted relay)
W>M  PASS rc 0, 295 s, sha256 equal, path direct
L>W  FAIL rc 137 after 7 s: lanex2-l's container has a 1 GiB memory limit; hp dmesg shows the memcg OOM kill (fleet environment, not a product result)
L>M  FAIL rc 1 after 892 s: file_transfer_stream_unavailable (relay path), support_3326cf58-9d0f-42d2-ab1d-5c998f16ec46
```
Own machines (dev-cab625433 / b239e6f32): L>W started (0.9 MB/s direct) and L>M (0.27 MB/s through the hosted relay), both cancelled by me at 6 min; the L>W rerun on b239e6f32 is in section 8. 0.3 GB L>W passed in part 2.

## 2. Unlimited above 4 GiB, 4,500,000,000 bytes, direct (own machines)
```
M>W  dev-cab625433: PASS, delivered, sha256 equal (8d1393...), 702 s, path direct; NTFS receiver
M>W  dev-b239e6f32: PASS, sha256 equal (8d1393...), 4.5 GB finished although the receiver's daemon (xr37-w) and then the sender's daemon (xr37-m) were restarted mid-transfer (section 5)
```

## 3. "Waiting for a direct connection or your relay" (UDP blocked on the sender: Windows firewall rule, Linux iptables, macOS pf)
```
W sender (W>L, W>M): message printed ("only a Paperboat relay reaches this machine now"), no bytes moved
M sender (M>W, pf block verified: path regional_relay): message printed, the transfer list shows no new transfer and no committed bytes for 103 s;
   after the block was removed: path direct, transfer uploading with 111 MB committed 15 s later, 4.5 GB later finished (PASS, resumes)
L sender (L>M through the hosted relay): "Waiting ... this machine is not reachable now" then "... only a Paperboat relay reaches this machine now" (PASS)
W>L and W>M after the block was removed: path stayed regional_relay 17 minutes and 6 minutes (W>L, W>M); `pb service restart` on W changed nothing (FAIL: no resume).
   W>M is asymmetric: `pb ping` M>W says direct, W>M says regional_relay; the Windows runner never had a direct path to the Mac runner or to the hp container. Not a block-removal artifact only (no baseline path was direct), cause not found (Haiku suggestions: the one-shot endpoint advertisement, engine_backend.go:258-259/295-297; unconfirmed).
```

## 4. Restart or daemon stop during a transfer
```
receiver daemon restarted during M>L (Linux receiver, exFAT Inbox, Unlimited 4.5 GB): offset kept growing (227 MB -> 234 -> 250 -> 269 -> ... 360 MB), PASS
receiver daemon restarted during M>W (Windows receiver): offset 490 MB -> 677 -> 842 -> 1.0 GB -> ... 2.1 GB, PASS
sender daemon restarted during M>W (macOS sender, 3.08 GB committed): 3.2 GB, 3.4 GB, ... finished, sha256 equal, PASS
sender daemon restarted during L>M (Linux sender through the hosted relay, 2.18 GB committed, dev-cab625433): offset frozen at 2,185,232,384 for 5+ minutes, the sender printed the Waiting lines only, ended canceled. FAIL (same as part 2: Linux sender restart); not reproduced when the path is direct: M sender above, and the Linux sender L>W (direct, 2 GB Automatic, dev-b239e6f32): daemon restarted at 159 MB, offset 199 -> 246 -> 291 -> 327 MB, resumes. So the hang needs a path that stays relay-only after the restart (same family as section 3).
```
Not run: the sender or receiver going to sleep (hosted runners cannot sleep; the real Windows PC is the shared fleet host).

## 5. Account setting applies to every OS (PATCH /v1/me/preferences through the console session of the own account)
```
mode automatic, plain `pb send` (no flag) of 2,000,000,001 bytes:  L, M, W each refuse: "This transfer is 2.0 GB. Automatic sends up to 2 GB. Use --unlimited ... Nothing was sent."
mode unlimited (patch 200), the same plain send:                   L (Linux), M (macOS), W (Windows) each accept it: transfer uploading (L, M) or "Waiting for a direct connection or your relay" (W, relay only)
mode set back to automatic.
```

## 6. Full disk (Inbox on a 100 MB volume; M and W as senders; Automatic)
```
L receiver, tmpfs 100 MB:  150 MB send refused at once: "xr37-l has no room for this in its Inbox (needs 143.1 MiB, 100 MiB free) ... Nothing was delivered."; Inbox empty. PASS
M receiver, APFS 100 MB disk image (case-insensitive): "xr37-m has no room ... (needs 143.1 MiB, 98.9 MiB free)"; Inbox empty. PASS
W receiver, NTFS 300 MB VHD: "xr37-w has no room ... (needs 381.5 MiB, 283.8 MiB free)"; Inbox empty. PASS
disk filled while the file is in flight (90 MB / 200 MB sends, the filler takes the rest): L, M, W each: "ran out of room in its Inbox while receiving, so nothing was delivered"; no file in the Inbox. PASS
```
Observed, not fixed: after such a failed publish the receiver's transfer record says `published` and the spooled `.content` stays until retention (L: 90 MB per failed send). The sender and the Inbox are right.
(A first attempt on L hung because I had mounted the tmpfs after the machine runtime started, which runs in its own mount namespace and published into its own copy; setup error, redone with the volume mounted in that namespace.)

## 7. File system limits
```
FAT32 Inbox (Linux receiver, loop-mounted 6 GB vfat), 4.5 GB Unlimited send, dev-cab625433: NO refusal. The spool took 30 MB, then 160 MB with the transfer still "uploading" (it would have copied to the end and failed at publish). FAIL -> fixed in b239e6f32 (Create refuses, new code file_too_large, tests TestCreateRefusesAFileTheInboxFileSystemCannotHold, TestFileTooLargeIsAnUnretryable413WithTheLimit, TestFileTooLargeIsPermanentAndCarriesTheLimit, TestSendOfAFileTheInboxFileSystemCannotHoldNamesTheLimit, TestMaxFileIsUnlimitedOnTheTestDisk; CI run 43409697103 and full cli run 43411304789 green).
FAT32 Inbox, same send on dev-b239e6f32: refused at once, before a byte moved: "xr37-l keeps its Inbox on a FAT volume, which cannot hold a file over 4096.0 MiB (this one is 4291.5 MiB). Send a smaller file, or point its Inbox at a disk with NTFS, exFAT, APFS or ext4 with `pb inbox set`. Nothing was sent." PASS
exFAT Inbox (Linux receiver, loop-mounted 6 GB exfat), same 4.5 GB send: accepted, uploading (result in section 8). 
Windows FAT32 volume (diskpart VHD, F:): `pb inbox set F:\Inbox` was refused with "inbox path must be owned by the current user" (dev-cab625433); on b239e6f32: "inbox path is on a FAT or exFAT volume, which cannot keep files private to you; choose a folder on an NTFS drive". PASS (message)
NTFS receiver: 4.5 GB file delivered, sha256 equal (section 2). 
```
Not run: macOS FAT32 or exFAT receiver (Unlimited needs a direct path into the hosted Mac runner and the senders only reached it through the relay), APFS receiver above 4 GiB, ext4 receiver above 4 GiB.
