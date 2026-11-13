# X-15 Windows parts, first pass on the shared fleet, 2026-11-13 08:36-09:02 IST

Machines: lanex2-l (Linux container on hp), lanex2-m (macOS, pbmac VM), lanex2-w (Windows runner winpool-3). All three `pb version`: dev-e4d2cc0f8 (cycle 2).
Driver: tools/device-smoke/xplat_send.py (existing, `--devices L,W` then `--devices M,W --only batch,modes,refuse,inboxset`); results xplat-send-20261113T031817Z.json, xplat-send-20261113T033045Z.json.

| Part | L>W | W>L | M>W | W>M |
|---|---|---|---|---|
| batch: ten files equal sha256, eleven refused naming the limit of 10 | PASS | PASS | FAIL once (xplat-b09.bin missing: ran while another job used the runner; rerun hit the runner's ssh tunnel closing, not rerun cleanly) | PASS |
| modes: no setuid/group/other/exec from sender 4755/0755/0777/0600; Windows receiver owner-only ACL | PASS (rerun by hand, first run `banner exchange` ssh timeout) | PASS | PASS | PASS |
| folder + symlink refused naming path and what to do | PASS | PASS | PASS | PASS |
| `pb inbox set`: relative and other-OS form refused, `~`, spaces/non-ASCII, other drive `D:\`, share `\\localhost\c$\...` accepted and each delivered | PASS (incl. D: and share) | PASS | PASS (incl. D: and share) | PASS |
| resume (receiver daemon restarted mid-send) | passed 2026-11-12 | NOT PASSED: 590 s without finishing (earlier run 3937 s) | not run | not run |

Not done: team file request approved on one OS received on W (both pairs), OneDrive/redirected-folder path (junction/share on the runner), W>L resume, M>W batch clean rerun, M<>W resume.
Blocker: at 09:01 IST the fleet's Windows runner winpool-3 died (ssh tunnel port 22203 refused, absent from `winpool.sh list`, expected end was 10:37); fleet W must be recreated by T-FLEET2.
W>L resume, diagnosis (read-only Haiku, hypothesis only, not reproduced): the sender's first credential renewal after the receiver drops has no deadline (cli/internal/filetransfer/client.go:781-790, shared/reattach/recovery.go:86-91, cli/internal/localapi/client_unix.go:219-248), and the Windows pipe read may never return. Needs a reproduction with sender-side logs on a Windows runner.
