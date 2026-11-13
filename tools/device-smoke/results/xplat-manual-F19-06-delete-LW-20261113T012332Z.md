# F19-06 delete variant, cells L and W (lane F1906, 2026-11-13)

Build: cycle 1, dev-aee62911e (pb-builds/dev-aee62911e), `pb version` = dev-aee62911e on both machines.
Fixture: team laner-sso (WorkOS test directory, SCIM driven by tools/device-smoke/scim_idp.py's Directory class), server and edges 2026.11.13.9.
Member D: a NEW throwaway directory user (not alice), synced in as a directory-managed team member by one reconcile (membership active, directory_user_id set).
Machines (D's own, shared into the team, owner-approved): Linux f1906-lx (pbvm2 container, machine_4e467a41), Windows f1906-win (winpool-13, machine_cfef9ba9). Each holds a team API token of D.
Owner side: the laner-sso owner profile, read-only checks and approvals.

Steps by hand, on every machine: before (team get ok, token API 200, owner sees both machines SHARED true, DB member active, tokens live, bindings active) -> DELETE the directory user (scim DELETE 204) -> poll -> after.

Result (all times UTC, from the run log below):
- Linux D profile first refused 13 s after the delete (one event poll).
- After: Linux and Windows `pb team get laner-sso` as D: permission refused; token API with D's team token: 401 from each machine; owner team get shows both machines SHARED false and 4 members; DB: membership active=f, all 4 of D's tokens revoked, both machine bindings active=f; D's personal machines still listed from both machines (personal account untouched).
- Owner and the other members untouched (owner readback 5 -> 4 member lines, only D gone).

Run log:

```
2026-11-13T01:20:28Z === BEFORE
2026-11-13T01:20:33Z LINUX D team get: TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	105
2026-11-13T01:20:40Z LINUX D token API: 200
2026-11-13T01:20:45Z WIN D team get: TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	105
2026-11-13T01:20:49Z WIN D token API: 200
2026-11-13T01:20:54Z OWNER team machines: f1906-lx personal:user_72cbab6f-b93e-44ba-8eb6-2ac2a970b7c3 online true true;f1906-win personal:user_72cbab6f-b93e-44ba-8eb6-2ac2a970b7c3 online true true;
2026-11-13T01:20:58Z OWNER members: 5 lines
2026-11-13T01:21:06Z D personal: lx=2 win=2 (own machines visible)
2026-11-13T01:21:08Z DB member | t | t
2026-11-13T01:21:08Z DB token | f1906-lx-probe | f
2026-11-13T01:21:08Z DB token | f1906-lx-t2 | f
2026-11-13T01:21:08Z DB token | f1906-lx-t3 | f
2026-11-13T01:21:08Z DB token | f1906-win-t | f
2026-11-13T01:21:08Z DB binding | machine_4e467a41-d146-441f-959a-da0bea1b40eb | t
2026-11-13T01:21:08Z DB binding | machine_cfef9ba9-f030-4dac-9348-98a13198937d | t
2026-11-13T01:21:08Z DELETE directory user at 2026-11-13T01:21:08Z
found True
delete 204
2026-11-13T01:21:21Z LINUX first refused after 13 s: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T01:21:41Z === AFTER
2026-11-13T01:21:45Z LINUX D team get: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T01:21:53Z LINUX D token API: 401
2026-11-13T01:21:58Z WIN D team get: pb.exe : pb: You do not have permission to perform this action. Check the selected account and target. Support 
2026-11-13T01:22:02Z WIN D token API: 401
2026-11-13T01:22:10Z OWNER team machines: f1906-lx personal:user_72cbab6f-b93e-44ba-8eb6-2ac2a970b7c3 online true false;f1906-win personal:user_72cbab6f-b93e-44ba-8eb6-2ac2a970b7c3 online true false;
2026-11-13T01:22:19Z OWNER members: 4 lines
2026-11-13T01:22:30Z D personal: lx=2 win=2 (own machines visible)
2026-11-13T01:22:33Z DB member | f | t
2026-11-13T01:22:33Z DB token | f1906-lx-probe | t
2026-11-13T01:22:33Z DB token | f1906-lx-t2 | t
2026-11-13T01:22:33Z DB token | f1906-lx-t3 | t
2026-11-13T01:22:33Z DB token | f1906-win-t | t
2026-11-13T01:22:33Z DB binding | machine_4e467a41-d146-441f-959a-da0bea1b40eb | f
2026-11-13T01:22:33Z DB binding | machine_cfef9ba9-f030-4dac-9348-98a13198937d | f
2026-11-13T01:22:33Z DONE
```
