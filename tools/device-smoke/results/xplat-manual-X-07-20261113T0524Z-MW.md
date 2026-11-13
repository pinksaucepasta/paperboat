# R-X-07: X-07 M<->W on the shared fleet build dev-051c1f7f2, 2026-11-13 ~10:50-11:10 IST

Machines: lanex2-m (macOS, the pbmac VM `admin` user) and lanex2-w (the real Victus Windows 11 PC, user Pujan). `pb version` on both (xplat_fleet.py, run right after): dev-051c1f7f2.
Scratch only: repository laner-x07-config-fixture, app dir laner-x07-app; run under flock /home/ubuntu/pb-wt/fleet-configsync.lock, config-sync.toml of both machines backed up and restored byte for byte (restore M identical, restore W identical; W had no machine file before, none after).
Run: xplat_config.py --only X-07 --devices M,W, result results/xplat-config-20261113T052416Z.json, 6/6 PASS:
- x07_paths M>W, W>M: drive-letter and `~\\x` local_path accepted on Windows (validate), refused on macOS with a message naming the rule and "work only on Windows"; no literal file created on the Mac.
- x07_unicode M>W, W>M: NFD name (as macOS writes it) plus a CJK/umlaut name arrive as one file per name on both, an edit on the peer round-trips, no conflict in status.
- x07_long_path M>W, W>M: a 390-character destination path arrives byte-identical on Windows.
