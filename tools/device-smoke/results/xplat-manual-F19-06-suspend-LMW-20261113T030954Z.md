# F19-06 suspend variant, cells L, M, W (lane F1906S, 2026-11-13)

Build: cycle 2, dev-e4d2cc0f8 (pb-builds/dev-e4d2cc0f8), `pb version` = dev-e4d2cc0f8 on the Linux container (pbvm2), the Windows runner (winpool-13) and the Mac (macpool-1, macOS 26.7.2 arm64). Server and edges 2026.11.13.9.
Fixture: team laner-sso (WorkOS test directory, SCIM driven by tools/device-smoke/scim_idp.py's Directory class). Owner side: the laner-sso owner profile (approval, grant and read-only checks).
Members: two NEW throwaway directory users (D for Linux + Windows, D2 for the Mac, because the Mac runner arrived late), each synced in as a directory-managed member (membership active, directory_user_id set) with a team API token and the machine(s) shared into the team and owner-approved.
Steps by hand on every machine: before (team get ok, token API 200, owner sees the machine SHARED true, DB member active, token live, binding active) -> SUSPEND in the directory (PATCH active=false, 200) -> poll -> after.

Result (all times UTC, from the run logs below):
- First refusal 13 s after the suspend on Linux (one event poll) and 12 s on the Mac.
- After one reconcile: `pb team get laner-sso` as the member refused on Linux, Windows and Mac; token API with the member's team token 401 (was 200) on all three; DB member active=f, every token revoked, every machine binding active=f; the owner no longer lists the member nor the shared machines; the member's personal machines still listed from each machine (personal account untouched); owner and the other members untouched (D2 joining accounts for the owner member count staying 5 in the Linux/Windows run: D gone, D2 present).
- Cleanup: both directory users deleted, both accounts deleted, machines revoked by exact id, Windows and Mac uninstalled, Linux container removed, macpool-1 stopped, winpool-13 returned to spare.

## Linux + Windows run (member D)

```
2026-11-13T02:50:58Z === BEFORE
2026-11-13T02:51:01Z LINUX D team get: TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	130
2026-11-13T02:51:04Z LINUX D token API: 200
2026-11-13T02:51:08Z WIN D team get: TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	130
2026-11-13T02:51:12Z WIN D token API: 200
2026-11-13T02:51:14Z OWNER team machines: f1906s-win personal:user_2250803e-58b2-4c40-acf9-c2925d399e49 online true true;f1906s-lx personal:user_2250803e-58b2-4c40-acf9-c2925d399e49 online true true;
2026-11-13T02:51:17Z OWNER members: 5 lines
2026-11-13T02:51:22Z D personal: lx=2 win=2 (own machines visible)
2026-11-13T02:51:24Z DB member | t | t
2026-11-13T02:51:24Z DB token | f1906s-lx-t | f
2026-11-13T02:51:24Z DB token | f1906s-win-t | f
2026-11-13T02:51:24Z DB binding | machine_1510e10f-a0d9-492a-aeb7-24527b08c110 | t
2026-11-13T02:51:24Z DB binding | machine_61d25dd2-4045-4c37-96b1-78f51bfef016 | t
2026-11-13T02:51:24Z SUSPEND directory user at 2026-11-13T02:51:24Z
found True
suspend 200
2026-11-13T02:51:37Z LINUX first refused after 13 s: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T02:51:57Z === AFTER
2026-11-13T02:51:59Z LINUX D team get: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T02:52:02Z LINUX D token API: 401
2026-11-13T02:52:06Z WIN D team get: pb.exe : pb: You do not have permission to perform this action. Check the selected account and target. Support 
2026-11-13T02:52:09Z WIN D token API: 401
2026-11-13T02:52:12Z OWNER team machines: 
2026-11-13T02:52:14Z OWNER members: 5 lines
2026-11-13T02:52:22Z D personal: lx=2 win=2 (own machines visible)
2026-11-13T02:52:23Z DB member | f | t
2026-11-13T02:52:23Z DB token | f1906s-lx-t | t
2026-11-13T02:52:23Z DB token | f1906s-win-t | t
2026-11-13T02:52:23Z DB binding | machine_1510e10f-a0d9-492a-aeb7-24527b08c110 | f
2026-11-13T02:52:23Z DB binding | machine_61d25dd2-4045-4c37-96b1-78f51bfef016 | f
2026-11-13T02:52:23Z DONE
```

## Mac run (member D2)

```
2026-11-13T03:05:12Z A. mac setup (macpool-1)
2026-11-13T03:05:33Z mac binary sha: ddd64ddc1956425d
2026-11-13T03:05:45Z install: Installed Paperboat dev-e4d2cc0f8 at /Users/runner/.local/bin/pb. Background update checks: false. Enrollment is unchanged.
2026-11-13T03:05:48Z mac version:   ▄▄▝▀▀▀▀▀▘▄▄   Version dev-e4d2cc0f8
2026-11-13T03:06:43Z mac machine machine_052f7634-7f5d-4c8d-b594-8b9e592e2fba
Accepted machine machine_052f7634-7f5d-4c8d-b594-8b9e592e2fba for team laner-sso.
2026-11-13T03:07:09Z === BEFORE
2026-11-13T03:07:15Z MAC D team get: TEAM	laner-sso	OWNER	user_ad3f1abe-afc3-475a-b433-2d43f319e618	GENERATION	136
2026-11-13T03:07:18Z MAC D token API: 200
2026-11-13T03:07:21Z OWNER team machines: f1906s-mac personal:user_3f837d66-8a64-4fe1-8f75-e5c02c280c60 online true true;
2026-11-13T03:07:23Z OWNER members: 5 lines
2026-11-13T03:07:27Z D personal: mac=1 (own machines visible)
2026-11-13T03:07:29Z DB member | t | t
2026-11-13T03:07:29Z DB token | f1906s-mac-t | f
2026-11-13T03:07:29Z DB binding | machine_052f7634-7f5d-4c8d-b594-8b9e592e2fba | t
2026-11-13T03:07:29Z SUSPEND directory user at 2026-11-13T03:07:29Z
found True
suspend 200
2026-11-13T03:07:41Z MAC first refused after 12 s: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T03:08:01Z === AFTER
2026-11-13T03:08:05Z MAC D team get: pb: You do not have permission to perform this action. Check the selected account and target. Support reference: <ref>
2026-11-13T03:08:08Z MAC D token API: 401
2026-11-13T03:08:10Z OWNER team machines: 
2026-11-13T03:08:13Z OWNER members: 4 lines
2026-11-13T03:08:16Z D personal: mac=1 (own machines visible)
2026-11-13T03:08:18Z DB member | f | t
2026-11-13T03:08:18Z DB token | f1906s-mac-t | t
2026-11-13T03:08:18Z DB binding | machine_052f7634-7f5d-4c8d-b594-8b9e592e2fba | f
2026-11-13T03:08:19Z DONE
```
