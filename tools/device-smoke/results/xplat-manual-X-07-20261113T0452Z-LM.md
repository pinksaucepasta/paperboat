# R-X-07: X-07 L<->M on the shared fleet build dev-051c1f7f2, 2026-11-13 ~10:20-10:35 IST

Machines: lanex2-l (Linux container laneX2-l on hp) and lanex2-m (macOS, the pbmac VM `admin` user). `pb version` on both (xplat_fleet.py status, 09:5x IST): dev-051c1f7f2, doctor online. Scratch only: repository laner-x07-config-fixture, app dir ~/.config|Library/Application Support/laner-x07-app; run under flock /home/ubuntu/pb-wt/fleet-configsync.lock, config-sync.toml of both machines backed up and restored byte for byte (restore L identical, restore M identical).
Run: xplat_config.py --only X-07 --devices L,M, result results/xplat-config-20261113T045157Z.json, 6/6 PASS:
- x07_paths L>M, M>L: drive-letter and `~\\x` local_path refused on both Unix machines with a message naming the rule and "work only on Windows"; no literal file created.
- x07_unicode L>M, M>L: an NFD name plus a CJK/umlaut name arrive on the peer as one file per name (NFC-compared), an edit on the peer round-trips, no conflict in status. (macOS keeps the name decomposed as written.)
- x07_long_path: n/a for a pair without a Windows machine.

History, for the record: two earlier full runs of this pair (not under the lock, same build) timed out in x07_unicode M>L ("lanex2-m sees the edit", 240 s); the same check alone passed twice (xplat-config-20261113T043526Z.json), a by-hand repro of the sequence (NFD created on M, edited on L) reached M in ~90 s, and the locked full run above passed. Not reproduced since; the failures coincided with other lanes working on the shared Mac.
