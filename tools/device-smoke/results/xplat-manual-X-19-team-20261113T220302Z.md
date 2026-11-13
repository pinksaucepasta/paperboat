# X-19: team private-access revocation — six OS pairs PASS

Direction is holder/member OS > shared target OS. Each mutation holds simultaneous byte-echoing `pb access machine` and raw `127.100.x.y` TCP flows; both must end with EOF/reset within 30 seconds. Fresh CLI and raw attempts must then be denied. Times include mutation and observation overhead and bound both transports.

| Pair | Grant revoke (s) | Setting off (s) | Result |
|---|---:|---:|---|
| L>M | 4.31 | 2.71 | PASS |
| M>L | 9.69 | 5.41 | PASS |
| L>W | 4.21 | 2.83 | PASS |
| W>L | 10.95 | 5.69 | PASS |
| M>W | 9.98 | 5.24 | PASS |
| W>M | 10.65 | 5.80 | PASS |

All four machines report `pb version` = `dev-ac409910b`: rx19d-a/b (Linux arm64 systemd containers on anvit), rx19d-m (macpool-3, macOS arm64), rx19d-w (winpool-5, Windows amd64). Binary source: `/home/ubuntu/pb-builds/dev-ac409910b/`; no device build was made. No shared fleet or apple installation was touched.

Throwaway paid team `rx19d-team` was created as scoped test data following `server/internal/testfixtures/service.go:92`, with two existing controlled test accounts. Only exact own records were changed. Grant revoke replaces exec+private_access with exec; setting-off retains the grant and disables team private access. Origins run independently of team authorization, so closing them cannot manufacture a passing result.

Fresh raw denial differs by platform: Linux rejects/EOF; macOS PF drops a SYN for a removed lease (`cli/internal/machineguard/server_darwin.go:302`); Windows WFP returns WSAEACCES 10013 (`cli/internal/machineguard/wfp/paperboat_windows.go:100`). A timeout after a raw connection has been established is never accepted, nor is silence on a held flow.

The initial harness treated Mac connect-drop and Windows explicit access denial as failures. These were harness errors; corrected platform/phase validation is covered by FreshRawDenialTest. ClosureTimingTest requires both transports, prior echo traffic, and the 30-second bound. No product bug or persistent daemon NotFound was reproduced.

Provenance: L>M is preserved from the first run (215345Z); the other five pairs use the corrected checkpointed run (220302Z). An intervening four-pair controller exited SIGTERM before final JSON; its observations are excluded. Harness `de44c7516`; tested binary `ac409910b`.

Validation: focused Python tests (2 PASS), `git diff --check`, and smoke harness CI https://github.com/pinksaucepasta/paperboat/actions/runs/43515822853 (PASS). No Go paths changed; no Go build/vet/test ran on oracle.

Cleanup: Mac and Windows uninstalled; four exact own machine IDs revoked/removed; team rx19d-team deleted and test entitlement removed; anvit rx19d-a/b containers deleted; macpool-3 released; winpool-5 stopped.

Submission blocked: `tools/push.sh` refused with `Branch not protected (HTTP 404)` / `main must require PRs and CI for admins first`. No bypass or direct push. Commits remain local for the parent to submit once protection is restored.

CHECKLIST.md is unchanged for the parent to update.

Skipped/not checked: no whole smoke suite or Go package tests; task-specific six-pair matrix and smoke harness tests passed.
