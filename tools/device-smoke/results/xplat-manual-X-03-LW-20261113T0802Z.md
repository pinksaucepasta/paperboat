# X-03 L<->W (lane R-X-03), 2026-11-13 ~13:10-13:45 IST

Machines (own trio, one account, never the shared fleet): laner-x03-l (Linux container on hp, Ubuntu systemd) and laner-x03-w (hosted Windows runner winpool-2). `pb version` on both, read after the runs: dev-d8c123c51 (main d8c123c51, /home/ubuntu/pb-builds/dev-d8c123c51).
Harness: tools/device-smoke/xplat_config.py on the controller, existing checks plus the new x03_eol_edit (added in this commit); repository, credentials and assignments are created and removed by the harness (fixture laner-x03-config-fixture, app dir laner-x03-app).

Run 1 (stdout /home/ubuntu/pb-wt/r-x-03-run/lw.out, the process was cut by the 30 min job limit, so no JSON): 
- PASS X-03:x03_bytes:L>W 114s sha256 equal on laner-x03-w and in the repository for 5 line-ending shapes
- PASS X-03:x03_bytes:W>L 21s sha256 equal on laner-x03-l and in the repository for 5 line-ending shapes
- FAIL X-03:x03_merge:L>W 326s: timed out waiting for merged result everywhere @ wait:123 <- x03_merge:357 <- <lambda>:519 <- check:251
- PASS X-03:x03_merge:W>L 157s three-way merge kept CRLF on every line
- FAIL X-03:x03_eol_edit:L>W 494s: timed out waiting for the conflict is listed on both devices @ wait:123 <- x03_eol_edit:381 <- <lambda>:519 <- check:251
Run 2 after the same three-way/conflict checks were given a 600 s wait (a merge needs the 60 s poll plus the 5 min minimum push interval; 240 s was too short), results/xplat-config-20261113T080214Z.json:
- PASS X-03:x03_merge:L>W 268s three-way merge kept CRLF on every line
- PASS X-03:x03_merge:W>L 242s three-way merge kept CRLF on every line
- PASS X-03:x03_eol_edit:L>W 314s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written
- PASS X-03:x03_eol_edit:W>L 234s line-ending-only edit travelled alone; against an edit on the other side it conflicted and both files stayed as written

Parts (both directions each):
- x03_bytes: CRLF, LF, mixed endings, no final newline and a lone CR arrive sha256-equal on the peer and in the repository (W>L and L>W).
- x03_merge: independent edits to line 1 (one side) and line 5 (other side) of a CRLF file merge to a file with 5 CRLF and 0 bare LF.
- x03_eol_edit: a line-ending-only edit (CRLF->LF) travels alone and arrives byte-exact; made together with an edit of text on the other side it conflicts (listed by `pb config conflict list`) and both files keep the bytes written.
- Git autocrlf: the merge is the product's own line diff on \n-split lines (cli/internal/hostruntime/configsync/merge.go:257), no normalisation; the repository bytes were compared to the files (x03_bytes assertion "the repository changed the bytes").
