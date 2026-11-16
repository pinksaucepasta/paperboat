# X-26 macOS re-proof on one fleet build (lane FLEET4, 2026-11-16 22:00-22:20 IST)

Build dev-2fa46738f on lanex2-m (the pbmac Tart VM, sleeper) and lanex2-l2 (peer), driver of R-X-26-N (freeze every pb process of the sleeper with SIGSTOP and cut its pb network with a pf anchor for 240 s, then thaw; terminals both ways typed before / while frozen / after, plain preview, durable tunnel, one send in flight each way, held-session reattach, fresh send), under fleet-mac.lock with a 5 min cooldown. Times UTC.

## Run 1 (16:16-16:29Z): FAIL on the Mac-as-client parts
lanex2-m's interactive CLI login (session_2d0d798d, created 15:36Z, first token refresh due ~16:21Z) was revoked by the server at 16:21:16Z with revocation_reason refresh_replay, in the middle of the freeze (server/internal/auth/device.go:557-566). Terminal out, exec M>l2 and the M>l2 send then failed with "Your Paperboat session is no longer valid". The previous login (session_52e01bbf) died the same way at 15:11:03Z inside R-X-26-N's M run.
```
16:17:01 RESULT PASS terminal attach+type before: inl 0.5s
16:17:01 RESULT PASS terminal attach+type before: outl 0.0s
16:20:30 RESULT PASS offline seen from l after 82s: lanex2-m personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_manag
16:20:30 RESULT PASS offline reason from l lanex2-m personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_manag
16:20:30 RESULT PASS exec to frozen refused clearly from l (124, 'timeout')
16:24:12 RESULT PASS recovers: list online from l 1.9s after thaw
16:24:12 RESULT PASS recovers: exec l->S 2.4s after thaw
16:24:12 RESULT PASS recovers: tunnel serves 2.9s after thaw
16:24:12 RESULT PASS recovers: preview serves 3.7s after thaw
16:24:12 RESULT PASS recovers: term inl 4.3s after thaw
16:24:12 RESULT FAIL recovers: term outl Nones after thaw
16:24:12 RESULT FAIL recovers: exec S->l Nones after thaw
16:24:12 RESULT PASS terminal inl: typed-while-frozen ran once count=1
16:24:12 RESULT FAIL terminal outl: typed-while-frozen ran once count=0
16:24:23 RESULT PASS terminal reattach to the held session after wake replay has pre marker=True live after 0.5s
16:24:23 RESULT PASS send in flight P1->S send (S receiver frozen) rc=0 inflight_at_freeze=(True, True) ed8","committed_offset":60000000,"state":"published","result_code":"published","created_at":"202
16:24:23 RESULT FAIL send in flight S->P2 send (S sender frozen) rc=1 inflight_at_freeze=(True, True)  and retry; if the service is recovering, retry in a moment.","retryable":false,"state_changed":"u
16:24:26 RESULT PASS fresh send l->S after wake {"data":{"batch_id":"batch_fbf67316-bcfc-4365-934a-198d367f40e7","transfers":[{"transfer_id":"transfer_09ec402a-2fe1-464a-af79-73bcaab5afda","batch_id":
16:24:28 allpass False
```

## Run 2 (16:33-16:41Z, started 1 minute after a fresh login, session_28f38fc4): every part passes
It finished before that session's first refresh, so it does not exercise the refresh-replay bug; it is NOT counted as the clean proof (coordinator decision). Evidence only.
```
16:34:10 RESULT PASS terminal attach+type before: inl 0.5s
16:34:10 RESULT PASS terminal attach+type before: outl 0.0s
16:37:39 RESULT PASS offline seen from l after 58s: lanex2-m personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_manag
16:37:39 RESULT PASS offline reason from l lanex2-m personal offline (allowed to sleep) terminal,exec,managed_ssh,files,preview_manag
16:37:39 RESULT PASS exec to frozen refused clearly from l (124, 'timeout')
16:41:20 RESULT PASS recovers: list online from l 1.6s after thaw
16:41:20 RESULT PASS recovers: exec S->l 2.4s after thaw
16:41:20 RESULT PASS recovers: tunnel serves 2.7s after thaw
16:41:20 RESULT PASS recovers: preview serves 3.0s after thaw
16:41:20 RESULT PASS recovers: exec l->S 3.1s after thaw
16:41:20 RESULT PASS recovers: term inl 3.3s after thaw
16:41:20 RESULT PASS recovers: term outl 3.8s after thaw
16:41:20 RESULT PASS terminal inl: typed-while-frozen ran once count=1
16:41:20 RESULT PASS terminal outl: typed-while-frozen ran once count=1
16:41:30 RESULT PASS terminal reattach to the held session after wake replay has pre marker=True live after 0.5s
16:41:30 RESULT PASS send in flight P1->S send (S receiver frozen) rc=0 inflight_at_freeze=(True, True) e7","committed_offset":60000000,"state":"published","result_code":"published","created_at":"2026
16:41:30 RESULT PASS send in flight S->P2 send (S sender frozen) rc=0 inflight_at_freeze=(True, True) ommitted_offset":60000000,"state":"published","result_code":"published","created_at":"2026-11-16T1
16:41:34 RESULT PASS fresh send l->S after wake {"data":{"batch_id":"batch_066cea92-0679-41d5-96eb-2c44810c42af","transfers":[{"transfer_id":"transfer_65754f9e-6e82-4652-808b-0b5a3af42803","batch_id":
16:41:38 allpass True
```
