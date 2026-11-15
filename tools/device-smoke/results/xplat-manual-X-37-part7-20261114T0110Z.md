# X-37 part 7 by hand, UTC 2026-11-13 22:30 - 2026-11-14 01:10 (IST 04:00 - 06:40), lane R-X-37E

Build under test: dev-08d3c4462 (`pb version` on every machine below; fleet lanex2-l2/-m/-w installed by FLEET3 04:34-04:37 IST; the Mac daemon pin 997f2035... equals the sha256 of pb-darwin-arm64 of that build). Fixes made in this part: 027ea380f (exFAT/FAT Inbox: the finished copy is renamed into place when the file system has no hard links; before, os.Link failed with ENOTSUP/EPERM after the whole copy and the send restarted), 08d3c4462 (a published file's spool copy is renamed into the Inbox or removed in the same step; before it stayed 7 days: 16 GB on lanex2-m), d84c3dc52 (the sender's refusal text no longer doubles the full stop of the Windows OS error).
Cause of the earlier 2-6 MB/s and restart on the Mac exFAT receiver: the Mac VM's data volume was nearly full (5.9 GB free, 16 GB of retained spool) and every publish failed at the link after the copy (reproduced with a darwin test binary on the VM: copyToInbox copied 4.3 GB then `link ... operation not supported`). With room and the fix, the same receive runs at 12 MB/s.

## 1. exFAT receiver on the real Mac (fleet lanex2-m, 6 GB ExFAT sparse image as Inbox), Windows victus sender, Unlimited 4,300,000,000 B, path direct (all rc 0)
```
23:22:41Z +   0s M version ['ersion dev-08d3c4462']
23:22:43Z +   1s W version ['ersion dev-08d3c4462']
23:22:45Z +   3s pb ping W>M 35ms
sample 4: path=direct connect=12.85ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=12.85/44.68/125.84ms
23:22:45Z +   3s orig inbox path: /Users/admin/Paperboat/Inbox
23:22:46Z +   5s setup (0, '/dev/disk7s1        \tWindows_NTFS                   \t/Volumes/R37EEX\n/dev/disk7s1 on /Volumes/R37EEX (exfat, local, nodev, nosuid, noowners, noatime,\n/dev/disk2s5    55Gi    14Gi    22Gi    40%    365k  226M    0%   /System/Volu
23:22:46Z +   5s inbox set /Volumes/R37EEX/Inbox
23:22:46Z +   5s inbox/spool: 
23:23:46Z +  65s inbox/spool: spool 674234368 transfer_4a7b0e5c-5120-4449-a64e-a5d8e5cd3d79.part
23:24:47Z + 125s inbox/spool: spool 1530920960 transfer_4a7b0e5c-5120-4449-a64e-a5d8e5cd3d79.part
23:25:47Z + 185s inbox/spool: spool 2292187136 transfer_4a7b0e5c-5120-4449-a64e-a5d8e5cd3d79.part
23:26:47Z + 245s inbox/spool: spool 3125805056 transfer_4a7b0e5c-5120-4449-a64e-a5d8e5cd3d79.part
23:27:47Z + 305s inbox/spool: spool 3981443072 transfer_4a7b0e5c-5120-4449-a64e-a5d8e5cd3d79.part
23:28:31Z + 350s send rc 0 elapsed 345s
23:28:31Z + 350s send out: r-x-37e-4300.bin: delivered to /Volumes/R37EEX/Inbox/r-x-37e-4300.bin on lanex2-m
Sending to lanex2-m (machine_9e1851b7-5763-40ab-9834-720becc3eb6d)
23:28:41Z + 359s verify: total 8398464 | -rwx------  1 admin  staff  4300000000 Nov 13 23:28 r-x-37e-4300.bin | e3502136045ad3cd325e629e3a3618b8189b14b2ee2013a256f0681cdeea586d |        3 | 0 |   0B	/Users/admin/Library/Application Support/Paperboat/runtime/fi
23:28:41Z + 359s inbox reset /Users/admin/Paperboat/Inbox
23:28:41Z + 360s inbox path after /Users/admin/Paperboat/Inbox
23:28:42Z + 360s cleanup (0, '/dev/disk2s5    55Gi    14Gi    21Gi    41%    365k  216M    0%   /System/Volumes/Data', '')
done-run 04:58:42
rc=0
```
sha256 e3502136...586d on both ends, 345 s, Inbox holds the one file, no .publish leftover, the Mac spool is 0 B (no .content), Inbox restored with `pb inbox reset`, image deleted.

## 2. APFS receiver on lanex2-m (default Inbox), same 4.3 GB W>M
```
01:03:12Z +   0s M version ['ersion dev-08d3c4462']
01:03:14Z +   1s W version ['ersion dev-08d3c4462']
01:03:17Z +   4s pb ping W>M le 4: path=regional_relay connect=69.98ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=69.98/304.37/906.48ms
01:03:17Z +   4s orig inbox path: /Users/admin/Paperboat/Inbox
01:03:17Z +   4s default inbox /Users/admin/Paperboat/Inbox
01:09:27Z + 375s send rc 0 elapsed 370s
01:09:27Z + 375s send out: r-x-37e-4300.bin: delivered to /Users/admin/Paperboat/Inbox/r-x-37e-4300.bin on lanex2-m
Sending to lanex2-m (machine_9e1851b7-5763-40ab-9834-720becc3eb6d)
Waiting for a direct connection or your relay: this machine is not reachable now
01:09:37Z + 385s verify: -rw-------  1 admin  staff  4300000000 Nov 14 01:09 /Users/admin/Paperboat/Inbox/r-x-37e-4300.bin | e3502136045ad3cd325e629e3a3618b8189b14b2ee2013a256f0681cdeea586d | 0 | 0 |   0B	/Users/admin/Library/Application Support/Paperboat/runt
01:09:37Z + 385s inbox path after /Users/admin/Paperboat/Inbox
01:09:37Z + 385s cleanup (0, '/dev/disk2s5    55Gi    14Gi    20Gi    42%    366k  214M    0%   /System/Volumes/Data', '')
done-run 06:39:37
rc=0
```
sha256 equal (e3502136...586d), 370 s, spool 0 B, no .publish leftover.

## 3. The Mac as sender (fleet), short sends both modes and Unlimited 4.3 GB to Windows
```
00:33:10Z +   0s M version ['ersion dev-08d3c4462']
00:33:11Z +   1s W version ['ersion dev-08d3c4462']
00:33:11Z +   1s L version ['ersion dev-08d3c4462']
00:33:15Z +   5s 1MB L plain rc 0 4s sha equal | r-x-37e-1MB: delivered to /home/tester/Paperboat/Inbox/r-x-37e-1MB on lanex2-l2 Sending to lanex2-l2 (machine_8139eee2-c661-484a-b1de-43be3f25e126)
00:33:21Z +  11s 1MB L unlimited rc 0 6s sha equal | r-x-37e-1MB: delivered to /home/tester/Paperboat/Inbox/r-x-37e-1MB on lanex2-l2 Sending to lanex2-l2 (machine_8139eee2-c661-484a-b1de-43be3f25e126) Waiting for a direct connection or your relay: only 
00:33:27Z +  17s 1MB W plain rc 0 6s sha equal | r-x-37e-1MB: delivered to C:\Users\Pujan\Paperboat\Inbox\r-x-37e-1MB on lanex2-w Sending to lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1)
00:33:35Z +  25s 1MB W unlimited rc 0 7s sha equal | r-x-37e-1MB: delivered to C:\Users\Pujan\Paperboat\Inbox\r-x-37e-1MB on lanex2-w Sending to lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1) Waiting for a direct connection or your relay: this 
00:34:15Z +  65s 100MB L plain rc 0 38s sha equal | r-x-37e-100MB: delivered to /home/tester/Paperboat/Inbox/r-x-37e-100MB on lanex2-l2 Sending to lanex2-l2 (machine_8139eee2-c661-484a-b1de-43be3f25e126)
00:34:55Z + 105s 100MB L unlimited rc 0 39s sha equal | r-x-37e-100MB: delivered to /home/tester/Paperboat/Inbox/r-x-37e-100MB on lanex2-l2 Sending to lanex2-l2 (machine_8139eee2-c661-484a-b1de-43be3f25e126)
00:35:08Z + 119s 100MB W plain rc 0 14s sha equal | r-x-37e-100MB: delivered to C:\Users\Pujan\Paperboat\Inbox\r-x-37e-100MB on lanex2-w Sending to lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1)
00:35:23Z + 133s 100MB W unlimited rc 0 13s sha equal | r-x-37e-100MB: delivered to C:\Users\Pujan\Paperboat\Inbox\r-x-37e-100MB on lanex2-w Sending to lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1)
00:35:48Z + 158s big file sha 6a85f316db0d9823
00:41:19Z + 489s big W unlimited rc 0 331s sha equal | r-x-37e-4300: delivered to C:\Users\Pujan\Paperboat\Inbox\r-x-37e-4300 on lanex2-w Sending to lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1)
00:41:22Z + 492s W spool after big send: transfer_16e83456-bb03-4cff-abad-b851b3faed0c.content 2000000000 | transfer_552e20fe-305e-4904-8e20-38964b8cc049.content 4500000000 | transfer_8c24d8b9-8279-4aca-ba47-70738845b30a.content 419430400 | transfer_9e7e2c8
00:41:22Z + 492s cleanup done (0, '0', '')
done-run 06:11:22
rc=0
```
Windows spool after the big send: the listed .content files are from earlier days (04:52Z-19:48Z 2026-11-13); none from 00:41Z, so the 4.3 GB receive left no spool copy on NTFS either. Not run: M>L above 4 GiB (the Linux fleet container sits behind anvit's uplink, ~25 min), the 4.3 GB sender and receiver cases are covered by W>M and M>W.

## 4. Write-protected Windows volume (hosted Windows runner winpool-5, NTFS VHDX attached read-only as R:, Inbox R:\Inbox, sender a Linux container)
`pb inbox set R:\Inbox` on the read-only volume: `pb: Paperboat cannot use R:\Inbox as the Inbox (chmod R:\Inbox: The media is write protected.). Choose a folder that exists or that you can create, and that only you can write to.` Inbox set while writable, volume re-attached read-only, 1,000,000 B send: exit 1 in 7.90 s, `r-x-37e-w could not store the file in its Inbox: The media is write protected.. Nothing was delivered. Fix that disk or point its Inbox at another one with `pb inbox set`, then send again.` (the doubled full stop is fixed in d84c3dc52, TestSendToAnInboxTheReceiverCannotWriteNamesTheOSError); record failed/storage_unavailable, no partial file and no .publish on the volume; Inbox restored, VHDX deleted. Windows as sender 1 MB and 100 MB, Automatic and Unlimited, to Linux: four sha256 pairs equal. Transcript: R-X-37E sub-agent W-report.md (pb version dev-08d3c4462 on both machines).

## 5. Linux, the sleeping stand-in on dev-08d3c4462 (two containers on pbvm2, direct, Unlimited 4.5 GB)
Receiver container `docker pause`d 120 s at 0.8 GB: sender printed `Waiting for r-x-37e-l2 to come back online (up to 32 min)`, growth resumed ~17 s after unpause, delivered, sha256 equal. Sender container paused 120 s at 1.0 GB: resumed ~10.5 s after unpause, delivered, sha256 equal.
FAT32 (msdos) Inbox refusal of 4.5 GB before a byte: PASS on the earlier fleet build dev-e4515714f (part 6); the re-run on this build came from a hosted Mac that turned out to be shared with another lane and is discarded (failures.md).

## 6. Linux sender freeze rerun (part 4 E3) on dev-6fe9473fb, pbvm2 containers, direct baseline, 2.5 GB Unlimited: UDP blocked +20 s, sender daemon restarted +42 s, block removed +62 s: bytes flowing again 4.6 s later, delivered rc 0, sha256 equal. One run, PASS; not a bug on this build.

## 7. Discarded and open
Discarded: the hosted Mac (macpool-4) results of the sub-agent run (spool kept after publish, every send file_transfer_unavailable): the runner was adopted by another lane at the same time, the running daemon pin ad79ae35 was not this build's pin.
Open for [x] (sleep): a Mac or Windows machine sleeping during a transfer: the pbmac VM cannot sleep (pmset error 0xe00002e2, X-26), hosted runners cannot sleep, victus must not. Also not run: Unlimited above 4 GiB with Linux as receiver from W or M, M>L above 4 GiB.
