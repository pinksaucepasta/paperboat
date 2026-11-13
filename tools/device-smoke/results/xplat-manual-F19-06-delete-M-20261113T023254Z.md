# F19-06 delete variant, cell M (lane F1906, 2026-11-13)

Build: cycle 1, dev-aee62911e (pb-builds/dev-aee62911e/pb-darwin-arm64, sha256 c3aae06b2e97cd37...), `pb version` on the Mac = dev-aee62911e. Mac: macpool-5 (GitHub-hosted macOS 26.7.2 arm64, user runner).
Fixture: team laner-sso (WorkOS test directory), server and edges 2026.11.13.9. Owner side: the laner-sso owner profile (approval, read-only checks).
Member: a NEW throwaway directory user (third one; the first two were used for Linux/Windows and an earlier Mac attempt whose token capture failed, see below), synced in by one reconcile as a directory-managed member, with one team API token and the Mac as a shared, owner-approved machine.

Result: before: Mac `pb team get` ok, token API 200, owner sees the Mac SHARED true, DB member active, token live, binding active. SCIM DELETE 204 -> Mac first refused 13 s later (one event poll). After: `pb team get` refused, token API 401, owner sees SHARED false and 4 members (5 before), DB member active=f, token revoked, binding active=f, member's personal machine still listed on the Mac.
The Mac was then uninstalled (pb uninstall) and its record is left for stale cleanup.

Earlier Mac attempt (second user, same steps, pbmac VM lease): refused after 11 s, membership/token/binding inactive in the DB, but the Mac-side token probe had no token (my script read the create response wrongly), so it is not counted; its machine record machine_fa0b319c is revoked/cleaned separately.

Run log:

```
2026-11-13T02:29:13Z new member state: member | t | t
2026-11-13T02:29:13Z A. mac setup (pool)
2026-11-13T02:29:33Z mac binary sha: c3aae06b2e97cd37
2026-11-13T02:29:41Z install: Installed Paperboat dev-aee62911e at /Users/runner/.local/bin/pb. Background update checks: false. Enrollment is unchanged.
2026-11-13T02:29:41Z mac pb path: /Users/runner/.local/bin/pb
2026-11-13T02:29:44Z mac version:   ▄▄▝▀▀▀▀▀▘▄▄   Version dev-aee62911e
2026-11-13T02:30:07Z mac machine machine_467a70a5-009b-4f93-ae5f-619b6664c0cc
TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	116
Accepted machine machine_467a70a5-009b-4f93-ae5f-619b6664c0cc for team laner-sso.
TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	119
2026-11-13T02:30:40Z === BEFORE
2026-11-13T02:30:45Z MAC D team get: TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	119
2026-11-13T02:30:48Z MAC D token API: 200
2026-11-13T02:30:58Z OWNER team machines: f1906-mac personal:user_8135cb09-23ff-4276-b8b7-cb6efe2ec19e online true true;f1906-mac personal:user_42a9dcc8-bb4c-4838-be43-3298e4d86e1d offline false false;
2026-11-13T02:31:04Z OWNER members: 5 lines
2026-11-13T02:31:10Z member personal: mac sees own machine rows=1
2026-11-13T02:31:12Z DB member | t | t
2026-11-13T02:31:12Z DB token | f1906-mac-t | f
2026-11-13T02:31:12Z DB binding | machine_467a70a5-009b-4f93-ae5f-619b6664c0cc | t
2026-11-13T02:31:12Z DELETE directory user at 2026-11-13T02:31:12Z
found True
delete 204
2026-11-13T02:31:25Z MAC first refused after 13 s: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T02:31:45Z === AFTER
2026-11-13T02:31:49Z MAC D team get: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T02:31:52Z MAC D token API: 401
2026-11-13T02:32:00Z OWNER team machines: f1906-mac personal:user_8135cb09-23ff-4276-b8b7-cb6efe2ec19e online true false;f1906-mac personal:user_42a9dcc8-bb4c-4838-be43-3298e4d86e1d offline false false;
2026-11-13T02:32:09Z OWNER members: 4 lines
2026-11-13T02:32:13Z member personal: mac sees own machine rows=1
2026-11-13T02:32:15Z DB member | f | t
2026-11-13T02:32:15Z DB token | f1906-mac-t | t
2026-11-13T02:32:15Z DB binding | machine_467a70a5-009b-4f93-ae5f-619b6664c0cc | f
Paperboat was completely removed. The Paperboat Inbox was preserved.
CPU usage: 1.15% user, 4.3% sys, 94.81% idle 
2026-11-13T02:32:23Z DONE
EXIT=0
```
