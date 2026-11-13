# X-03 L<->M (lane R-X-03), 2026-11-13 ~14:30-17:30 IST, NOT ticked

Machines (own trio): laner-x03-l (Linux container on hp) and laner-x03-m (hosted macOS runner, macpool-8). `pb version` on both: dev-cf0c0a620 (first runs), dev-cec6abcc4 (after the configsync fix cec6abcc4, read at install).

Passed in both directions (results/xplat-config-20261113T090320Z.json, results/xplat-config-20261113T110430Z.json):
- PASS X-03:x03_bytes:L>M 90s sha256 equal on laner-x03-m and in the repository for 5 line-ending shapes
- PASS X-03:x03_bytes:M>L 17s sha256 equal on laner-x03-l and in the repository for 5 line-ending shapes
- PASS X-03:x03_eol_edit:L>M 146s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written
- PASS X-03:x03_eol_edit:M>L 142s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written
- PASS X-03:x03_bytes:L>M 30s sha256 equal on laner-x03-m and in the repository for 5 line-ending shapes
- PASS X-03:x03_bytes:M>L 17s sha256 equal on laner-x03-l and in the repository for 5 line-ending shapes
- PASS X-03:x03_eol_edit:L>M 142s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written
- PASS X-03:x03_eol_edit:M>L 151s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written

Failed, repeatable (5 of 5 on L<->M, passes on L<->W): x03_merge. Independent edits on line 1 (one machine) and line 5 (other) of a CRLF file, made within one second of the file settling: the other machine merges and publishes A1+B5, this machine stays state=conflict reason concurrent_update with its own edit and never adopts the merged remote.
Cause found from the state files on laner-x03-l (probe3-lm: pull/baseline.json frozen path, pull baseline has no entry for the file, push baseline has it): the pull half (merge disabled, applies remote changes) never recorded the new file in its baseline (it only sees it agreed with the remote on the next 60 s poll; both edits happen before that), so its merge ancestor is empty and the already merged remote cannot be adopted (workspace_reconciler_merge.go mergeConflicts: no baseline entry -> not mergeable; mergesEqualToRemote only adopts a merge). Nothing is lost: both files keep their bytes and the conflict is listed.
Fix proposal (not done): the pull half treats a local file that equals what this machine's push half last published (push/baseline.json) as unchanged, so the remote merge result is applied.
Related bug found on the way and fixed in cec6abcc4 (test TestPullHalfLeavesAStaleFreezeWhenTheRemoteMovesToTheMerge): a conflict frozen by the pull half stayed frozen by path name when the remote later moved.
