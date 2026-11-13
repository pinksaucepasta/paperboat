# F5-03 on macOS, 2026-11-13 (IST), by hand with resume_check.py (plat lane)

Client: fleet Mac lanex2-m (pbmac Tart VM on apple, home network), target: Linux fleet container lanex2-l (hp), same account. `pb version` of both: dev-051c1f7f2 (checked 04:14Z; the fleet was upgraded to it at 03:47Z by T-FLEET2). Tree: 602 files, 407.5 MiB (600 files 2-32 KiB + two 200 MiB files); resume: a 200 MiB random file, client killed (SIGINT to the process group) while transferring, then re-run; hashes compared (sha256). Commands: pb scp -r, pb sftp -b (put/get -a), pb rsync -a (macOS rsync is openrsync 2.6.9-compatible: resume flags --partial --append; the interrupted run carries the same flags). No file contents printed.

## scp (first fleet run, after the 03:47Z upgrade) — f503-fleet.txt
PASS scp up large-tree ok 602 files 269s
PASS scp down large-tree ok 602 files 249s
PASS scp up resume interrupted=True (first run: killed after 3.0s; stat: 2872320) partial=2872320 of 209715200 bytes; resumed ok 74s; hash match=True
PASS scp down resume interrupted=True partial=6789120 of 209715200 bytes; resumed ok 69s; hash match=True

## sftp (rerun) — f503-fleet2.txt
PASS sftp up large-tree ok 602 files 202s
PASS sftp down large-tree ok 602 files 303s
PASS sftp up resume interrupted=True (first run: killed after 3.0s; stat: 2350080) partial=2350080 of 209715200 bytes; resumed ok 72s; hash match=True
PASS sftp down resume interrupted=True partial=6005760 of 209715200 bytes; resumed ok 95s; hash match=True

## rsync large tree (rerun) — f503-fleet2.txt
PASS rsync up large-tree ok 602 files 153s
PASS rsync down large-tree ok 602 files 143s

## rsync interrupted-then-resumed, both directions — f503-fleet9.txt (harness fix: the interrupted first run also carries --partial; before it, partial=None because plain rsync -a keeps nothing)
PASS rsync up resume interrupted=True (first run: killed after 40.0s; stat: 63766528) partial=63766528 of 209715200 bytes; resumed ok 130s; hash match=True
PASS rsync down resume interrupted=True partial=82837504 of 209715200 bytes; resumed ok 51s; hash match=True

## Notes: an earlier fleet run lost sftp/rsync downloads because another lane ran a runtime install on lanex2-l at 03:46:56Z (journal: install-supplied, services restarted); those parts were rerun above. First attempts on a hosted macpool runner failed because the shared account was at the 64 managed-SSH client key cap (logged in failures.md); sftp 4/4 also passed there on dev-aee62911e (macpool-6 → plat2-s, throwaway account).
