# X-37 part 2 by hand on my own Linux container (arm64, anvit) and hosted Windows runner, UTC 20261112 21:30 - 23:30 (IST 03:00 - 05:00 13 Nov)

Builds: main 7be86f072 plus the send fixes below (dev-sendfix). Account: a throwaway account.
## Fixed on the way: `pb send --unlimited` refused every send over 2 GB (b8c04bef9)
Found with a 2.2 GB file on the fleet and on my pair: refused in 1 s with the Automatic hint. Cause: files were prepared with the Automatic bound whatever the mode, and the later validation never used declaredTransferLimits. Test TestPrepareBoundFollowsTheTransferMode; anvit `go test ./cmd/pb -run Send|Transfer|...` ok. With the fix a 4.4 GB Unlimited send Windows to Linux started and reached 3.2 GB.
## Results of the other parts
```
Automatic 0.3 GB L>W:           PASS rc 0, 380 s, sha256 equal
Automatic 0.3 GB W>L:           FAIL after 917 s (= the 15 min send queue wait) with the destination-busy text ("... then send again"): the Linux receiver allows 2 concurrent batches and my earlier 4.4 GB W>L send still held slots; Spark read the code (send_storage.go:96,73, service_types.go:52). Not a send-path bug; not rerun on a clean receiver (the runner is gone).
Unlimited, UDP blocked on the sender (path regional_relay): PASS for the message: "Waiting for a direct connection or your relay: this machine is not reachable now" then "...: only a Paperboat relay reaches this machine now"
  after UDP returned: FAIL, still waiting 680 s later. Cause (certain): waiting generates no traffic that could find a direct path and WakePeer ran only for PathNone; fix 8f95a82b9 wakes the peer for a hosted relay too (unit test), NOT verified on devices (the runner died).
Unlimited, sender daemon restarted 25 s into a 1.5 GB send: FAIL, the send stayed on the waiting line for 19+ min, no daemon events after the restart, a second fresh `pb send` also did not start although `pb ping` said direct both ways. Spark could not name the mechanism (suspects a hung pooled connection or a missing lease renewal); not fixed.
4.4 GB Unlimited W>L: reached 3.2 GB at about 1.1 MB/s (runner uplink) when my harness timed out at 3000 s; not completed. L>W 4.4 GB: first try failed 91 s after a build swap ("temporarily unavailable"), not rerun.
```
## Not run
FAT32/exFAT/NTFS/APFS/ext4 receiver limits and a full disk, the account mode seen from every OS, any send involving the Mac.
