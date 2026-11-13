# X-19 team held-private-flow attempt — NOT A PASS

No CHECKLIST cell is tickable. None of the six pairs completed both mutations. Isolated W>L grant-revoke observations (6.99 s and 17.55 s) are excluded from whole-pair evidence.

## Machines and versions

- `lanex2-l`: `pb version` = `dev-79203d6e5`.
- `lanex2-m`: `pb version` = `dev-79203d6e5`.
- `lanex2-w`: `pb version` = `dev-cf0c0a620`.
- `rx19t-l`: `pb version` = `dev-d77a40713`.
- `rx19t-w`: `pb version` = `dev-d77a40713`.
- `rx19t-l-hp`: `pb version` = `dev-d77a40713`.

F owns throwaway team `rx19t-ce118da5`; Y is `user_0655d20c-85c7-4b54-b03a-cf1cb29bfe41`. Y devices: rx19t-l (pbvm2, initially), rx19t-l-hp (fallback on hp), rx19t-w (winpool-11). No own Mac was available; lane authorized X-21 inversion with the fleet Mac as member. W>L also uses Y Windows as owner, F Linux as member after Victus became unreachable.

Team/paid entitlement test data used the exact `testfixtures.Service.CreateTeam` INSERT design (`server/internal/testfixtures/service.go:92`), with an rx19t team id. The endpoint itself derives a fixed team id from F email and could affect a pre-existing shared fixture, so it was not called. Only this unique team and exact three own machine ids were cleaned up.

## Exact run commands

The private launcher loads the existing fleet environment and VICTUS_PASS privately from REMOTE_TESTING.md; neither value is recorded. It sets XPE_TEAM=rx19t-ce118da5, XPE_MEMBER_WIN_SSH=winpool-11; initially XPE_MEMBER_L_HOST=pbvm2/XPE_MEMBER_L_CT=rx19t-l with user pbt uid1000/sudo -n docker, then hp/rx19t-l-hp with docker. XPE_MEMBER_L_NAME=rx19t-l-hp after fallback.

```sh
python3 /tmp/rx19t-run.py --pairs "M>L,W>L,L>W,M>W"
python3 /tmp/rx19t-run.py --pairs "L>W"
python3 /tmp/rx19t-run.py --pairs "L>W,M>W,W>M"
python3 /tmp/rx19t-run.py --pairs "W>L,M>W,L>M,W>M,M>L"
python3 /tmp/rx19t-run.py --pairs "L>W" # first matrix stopped after this failure
python3 /tmp/rx19t-run.py --pairs "W>L" # final matrix stopped during cleanup after timeout
timeout 25 ssh -o ConnectTimeout=10 hp docker exec -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 laneX2-l /home/tester/.local/bin/pb version
python3 -m unittest discover -s tools/device-smoke -p test_xplat_team_private_end.py
```

The launcher calls xplat_team_private_end.main(). Per-flow command is `pb access machine --workspace rx19t-ce118da5 <owner-alias> --port <recorded origin_port> --listen 127.0.0.1:0 --json`; holder executes on the member. Origin is a bounded Python echo server through owner-account pb exec. Final harness uses detached CIM for Windows origins, retains exec while removing private_access, waits for server permissions plus daemon resolve before measuring, and checks both EOF/reset and the fresh-attempt refusal. These final refinements have unit validation but no complete matrix proof.

## Observations (failures and reruns excluded)

| Pair | Grant revoke | Setting off | Whole result |
|---|---|---|---|
| L>M | Not proved | Not proved | NOT PROVEN |
| M>L | Not proved | Not proved | NOT PROVEN |
| L>W | Not proved | Not proved | NOT PROVEN |
| W>L | Isolated 6.99 s / 17.55 s, permission named | Not proved | NOT PROVEN |
| M>W | Not proved | Not proved | NOT PROVEN |
| W>M | Not proved | Not proved | NOT PROVEN |

Embedded JSON `attempts` preserves all individual result objects, versions, errors and isolated PASS lines. Early private_access-only grants hid targets in server inventory; later server-visible targets still returned daemon NotFound. Haiku read-only code diagnosis found a plausible daemon topology readiness race, not a confirmed server-filter defect. No product code was changed.

W>L setting-off phase did not start a second held flow: machine-list command timed out (120 s). hp controller `pb version` then timed out at 25 s with no output; Victus SSH had already failed with connect timeout. Final matrix was interrupted after the repeated prerequisite failure; M>W/L>M/W>M were not reached. No unchanged device retries were counted as evidence.

## Cleanup

winpool-11 stopped. No Mac pool runner claimed. Exact own machine records revoked with cleanup_test_account.REVOKE SQL fence; own team marked deleted, allow_private_access restored false, grants and bindings inactive, Y membership inactive; throwaway Y account deleted through fresh_account.mjs (PASS). No fleet identity/install/account settings changed. Native test processes are bounded; local controller jobs stopped.

Remote cleanup attempts failed because hp/pbvm2 were unreachable. **Cannot claim nothing left running:** containers `rx19t-l-hp` on hp and `rx19t-l` on pbvm2 may remain; pbvm2 `~/laneh/rx19t-pb` also needs removal. The coordinator received the exact cleanup request. Their device records are revoked, so they cannot access the account.

Skipped/not checked: complete device PASS for all six pairs; final Windows CIM launch on devices; successful removal of unreachable containers. CHECKLIST.md unchanged; no push.
