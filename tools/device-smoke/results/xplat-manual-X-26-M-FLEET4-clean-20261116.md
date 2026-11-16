# X-26 macOS: one clean run on the final fleet build, across a token refresh (lane FLEET4, 2026-11-16 23:22-23:31 IST = 17:52-18:01Z)

Build dev-d57a6af85 on both machines (FIX-REFRESH CLI fixes d189bcf31 keychain cache + 8d20fe14f cached access token, SSH CA 5177c7857, guard Rev 2): lanex2-m (the pbmac Tart VM, sleeper, machine id machine_7cd4726b-92f2-4884-8b5a-ae40023764e2 after the 23:12 IST re-enrolment) and lanex2-l2 (peer). Driver of R-X-26-N, one run: every pb process of the sleeper SIGSTOPped (FROZEN 17:53:37Z) and the pb network cut by a pf anchor for 240 s, thaw ~17:57:37Z; under fleet-mac.lock with a 5 min cooldown. The machine's CLI login was session_147fc4ce (final-13c6615d7e, created 17:42:31Z, normal device-flow login).

## Token refresh inside the freeze
The login's access token expired at 17:57:32Z, i.e. during the freeze (it was due ~17:56:45Z). After the thaw the session was NOT revoked: server row session_147fc4ce is active (last_used 18:00:52Z, revoked_at empty), `pb auth status` on the Mac shows the access expiry moved to 18:12:38Z (token reissued ~17:57:38Z, right at the thaw) and `pb doctor` is healthy. The two earlier logins of this Mac died with revocation_reason refresh_replay at 15:11:03Z and 16:21:16Z inside freeze runs on dev-6e1e23255 / dev-2fa46738f.

## Every pb process on lanex2-m runs dev-d57a6af85 (checked 18:01Z, right after the run)
8 Paperboat processes: machineguard (root), session owner, carrier, hostd, hostd worker, user daemon, updated (root), runtime worker. Distinct executables, sha256 prefix 8d52ce65f65640f9 for all three, equal to the build's pb-darwin-arm64 (8d52ce65f65640f9c50d682e4e90aeedfb08cf43a98eb1628c3fa005632cfa2a):
- /Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb  -> 8d52ce65f65640f9 (pb version: dev-d57a6af85)
- /Library/PrivilegedHelperTools/Paperboat/users/u501/releases/pins/8d52ce65f65640f9c50d682e4e90aeedfb08cf43a98eb1628c3fa005632cfa2a/pb -> 8d52ce65f65640f9 (the only pin directory present)
- /Library/PrivilegedHelperTools/io.paperboat.machineguard -> 8d52ce65f65640f9 (pb version: dev-d57a6af85)
No pinned older release, no older build running (all processes started after the `pb reset` + `pb pair` of 23:12 IST).

## Run result: allpass True
```
17:53:25 RESULT PASS terminal attach+type before: inl 0.5s
17:53:25 RESULT PASS terminal attach+type before: outl 0.0s
17:56:55 RESULT PASS offline seen from l after 82s: lanex2-m personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_manag
17:56:55 RESULT PASS offline reason from l lanex2-m personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_manag
17:56:55 RESULT PASS exec to frozen refused clearly from l (124, 'timeout')
18:00:37 RESULT PASS recovers: tunnel serves 0.8s after thaw
18:00:37 RESULT PASS recovers: exec S->l 2.2s after thaw
18:00:37 RESULT PASS recovers: term outl 3.2s after thaw
18:00:37 RESULT PASS recovers: exec l->S 3.3s after thaw
18:00:37 RESULT PASS recovers: preview serves 3.4s after thaw
18:00:37 RESULT PASS recovers: term inl 5.7s after thaw
18:00:37 RESULT PASS recovers: list online from l 6.5s after thaw
18:00:37 RESULT PASS terminal inl: typed-while-frozen ran once count=1
18:00:37 RESULT PASS terminal outl: typed-while-frozen ran once count=1
18:00:48 RESULT PASS terminal reattach to the held session after wake replay has pre marker=True live after 0.5s
18:00:48 RESULT PASS send in flight P1->S send (S receiver frozen) rc=0 inflight_at_freeze=(True, True) 8d","committed_offset":60000000,"state":"published","result_code":"published","created_at":"2026
18:00:48 RESULT PASS send in flight S->P2 send (S sender frozen) rc=0 inflight_at_freeze=(True, True) ommitted_offset":60000000,"state":"published","result_code":"published","created_at":"2026-11-16T1
18:00:51 RESULT PASS fresh send l->S after wake {"data":{"batch_id":"batch_ee6b8c5a-1934-40a0-b0ee-c827d3846900","transfers":[{"transfer_id":"transfer_1196eecc-9363-4a8e-8abd-3cfb0c8f0ecd","batch_id":
18:00:55 allpass True
```
