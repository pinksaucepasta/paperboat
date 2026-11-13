# R-X-34: X-34 non-owner live refusal and API-token sign-in on L, M and W, 2026-11-13 (IST ~14:30-15:30)

Build: `pb version` dev-d8c123c51 (main) on every machine below. Server 2026.11.13.11.

Machines
- Owner, shared targets: the fleet `lanex2-l` (Linux container), `lanex2-m` (Mac), `lanex2-w` (Victus), owner `pb` run on lanex2-l (fleet account), team `paperboat-smoke-365ac02bcda9`. Nothing installed or changed on the fleet; only shares, grants and one saved team-policy version, each undone (policy restored to its start digest, machines unshared).
- Member account (second account, active member of that team) with three own clients, all `pb` dev-d8c123c51:
  - L: `xa2-l2` (Linux container on anvit, upgraded in place from dev-d24378a95 with its own `pb install`, identity unchanged)
  - W: `rx34-w` (winpool-9, Windows Server runner, installed with `pb install`, paired with a one-shot enrollment token, then browser-approved login)
  - M: `rx34-m` (macpool-1, macOS runner, installed with `pb install`, paired the same way, browser-approved login)
- A session signed in with an API token cannot create API tokens (by design); the token-creating sides use the browser-approved session.

## Part 1: a non-owner's live `pb exec` refusal, same text from L, M and W clients (all three targets)
For each target: (1) machine not shared, (2) shared with no rule, (3) `pb team machine grant ... --all-members --capability exec`, (4) a deny rule `{"src":["*"],"dst":["machine:<target>"],"deny":["exec"]}` saved, then the member runs `pb exec --workspace <team> <target> -- echo rx34` from each client and, in the deny state, `pb policy explain --src <self> --dst machine:<target> --action exec`. Raw output follows (email and support ids redacted).

Expected and seen on every target and every client: not shared / shared without rule / deny rule: exit 1 with the identical text `pb: No machine has that name or ID. Run \`pb machine list\` to see your machines. Support reference: support_<id>. ...` (the machine is invisible to a member who has no allowing rule, so a denied member learns nothing about it); with the grant: `rx34` and exit 0 (plus the ENV-pending notice); `explain` in the deny state: `DENIED: denied_by_rule` with `Rules: /rules/N` naming the deny rule, identical from all three clients.
Tallies over the passing set (lanex2-l and lanex2-w from the first pass, lanex2-m from the rerun): 27 of 27 refusals (3 targets x 3 clients x 3 states) with the identical text and exit 1, 9 of 9 grant executions allowed (`rx34`, exit 0), 9 of 9 explains `DENIED: denied_by_rule`.

Note on lanex2-m: in the first pass the grant had not reached the Mac within about 90 s (L client: "The target machine was reached but did not open the connection ... Retry in a minute"; W and M clients still saw "No machine has that name"); that is the documented asynchronous grant propagation (the check retries). The rerun with a longer bounded wait passed on all three clients; both passes are in this file.

### Targets lanex2-l and lanex2-w (and the first, transient lanex2-m pass)
```
## target lanex2-l (machine_7985e977-eaf8-4f0b-b0f9-3c0d22ed747a), team policy start version 88
### 1 not shared
[member client L]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 2 shared, no rule

$ owner: pb team machine share <team> lanex2-l
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
[member client L]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 3 grant exec to every member
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
[member client L]

$ pb exec --workspace <team> lanex2-l -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
[member client W]

$ pb exec --workspace <team> lanex2-l -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
[member client M]

$ pb exec --workspace <team> lanex2-l -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
### 4 deny rule in the team policy
Tests: 0 passed, 0 failed
Access changes:
- lost user:<member> machine:lanex2-l exec (/rules/4)
- lost user:<member> machine:lanex2-l exec (/rules/4)
Saved policy version 90 .
[member client L]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-l -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client L]
DENIED: denied_by_rule
Rules: /rules/5
[member client W]
DENIED: denied_by_rule
Rules: /rules/5
[member client M]
DENIED: denied_by_rule
Rules: /rules/5
### 5 cleanup
Saved policy version 91 .
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
## target lanex2-m (machine_9e1851b7-5763-40ab-9834-720becc3eb6d), team policy start version 91
### 1 not shared
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 2 shared, no rule

$ owner: pb team machine share <team> lanex2-m
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 3 grant exec to every member
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: The target machine was reached but did not open the connection. Its Paperboat service may still be starting, or it has not accepted this machine yet. Retry in a minute; run `pb doctor` if this continues. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 4 deny rule in the team policy
Tests: 0 passed, 0 failed
No access changes.
Saved policy version 94 .
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client L]
DENIED: denied_by_rule
Rules: /rules/4
[member client W]
DENIED: denied_by_rule
Rules: /rules/4
[member client M]
DENIED: denied_by_rule
Rules: /rules/4
### 5 cleanup
Saved policy version 95 .
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
## target lanex2-w (machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1), team policy start version 95
### 1 not shared
[member client L]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 2 shared, no rule

$ owner: pb team machine share <team> lanex2-w
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
[member client L]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 3 grant exec to every member
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
[member client L]

$ pb exec --workspace <team> lanex2-w -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
[member client W]

$ pb exec --workspace <team> lanex2-w -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
[member client M]

$ pb exec --workspace <team> lanex2-w -- echo rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
rx34
exit=0
### 4 deny rule in the team policy
Tests: 0 passed, 0 failed
Access changes:
- lost user:<member> machine:lanex2-w exec (/rules/4)
- lost user:<member> machine:lanex2-w exec (/rules/4)
Saved policy version 97 .
[member client L]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-w -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client L]
DENIED: denied_by_rule
Rules: /rules/5
[member client W]
DENIED: denied_by_rule
Rules: /rules/5
[member client M]
DENIED: denied_by_rule
Rules: /rules/5
### 5 cleanup
Saved policy version 98 .
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
ALLDONE
```

### Target lanex2-m, rerun with a longer grant wait
```
## target lanex2-m (machine_9e1851b7-5763-40ab-9834-720becc3eb6d), team policy start version 98
### 1 not shared
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 2 shared, no rule

$ owner: pb team machine share <team> lanex2-m
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
### 3 grant exec to every member
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
rx34
Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices.
exit=0
### 4 deny rule in the team policy
Tests: 0 passed, 0 failed
Access changes:
- lost user:<member> machine:lanex2-m exec (/rules/4)
- lost user:<member> machine:lanex2-m exec (/rules/4)
Saved policy version 100 .
[member client L]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client W]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client M]

$ pb exec --workspace <team> lanex2-m -- echo rx34
pb: No machine has that name or ID. Run `pb machine list` to see your machines. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit=1
[member client L]
DENIED: denied_by_rule
Rules: /rules/5
[member client W]
DENIED: denied_by_rule
Rules: /rules/5
[member client M]
DENIED: denied_by_rule
Rules: /rules/5
### 5 cleanup
Saved policy version 101 .
{"name":"paperboat-smoke-365ac02bcda9","suspended":false,"te
```

## Part 2: API token created on one OS, used from `pb` on another (`policy_token.py`, member account)
Each run: create a token scoped to exec on one machine, `pb login --api-token-file -` on the holder, exec on the scoped machine works, exec on another machine is outside the token's scope, a write outside the scope (creating a token) is refused with `outside_token_scope`, revoking the token ends the holder's session; then the tag:ci case (token scoped to `tag:ci`, use attributed to the token via last_used_at, removing the tag removes the access). Creator and holder cover every OS in both roles: L creates/W uses, W creates/M uses, M creates/L uses.

```
#### run 1: token created on L, used on W (allowed rx34-m, other xa2-l2)
PASS create a token scoped to exec on rx34-m
PASS pb login --api-token-file signs in with the token
PASS exec on the scoped machine works
PASS exec on another machine is outside the scope
PASS a write outside the scope is refused
PASS revoke the token
PASS revoking ends the token session
PASS tag the scoped machine tag:ci in the personal policy
PASS create a token that may only exec on tag:ci
PASS sign in with the tag:ci token
PASS exec on the tag:ci machine works
PASS the use is attributed to the token (last_used_at)
PASS restore the personal policy (the machine loses tag:ci)
PASS without the tag the token may not exec there
PASS revoke the tag:ci token
RESULT PASS
#### run 2: token created on W (browser-approved session), used on M (allowed xa2-l2, other rx34-w)
PASS create a token scoped to exec on xa2-l2
PASS pb login --api-token-file signs in with the token
PASS exec on the scoped machine works
PASS exec on another machine is outside the scope
PASS a write outside the scope is refused
PASS revoke the token
PASS revoking ends the token session
PASS tag the scoped machine tag:ci in the personal policy
PASS create a token that may only exec on tag:ci
PASS sign in with the tag:ci token
PASS exec on the tag:ci machine works
PASS the use is attributed to the token (last_used_at)
PASS restore the personal policy (the machine loses tag:ci)
PASS without the tag the token may not exec there
PASS revoke the tag:ci token
RESULT PASS
#### run 3: token created on M (browser-approved session), used on L (allowed rx34-w, other rx34-m)
PASS create a token scoped to exec on rx34-w
PASS pb login --api-token-file signs in with the token
PASS exec on the scoped machine works
PASS exec on another machine is outside the scope
PASS a write outside the scope is refused
PASS revoke the token
PASS revoking ends the token session
PASS tag the scoped machine tag:ci in the personal policy
PASS create a token that may only exec on tag:ci
PASS sign in with the tag:ci token
PASS exec on the tag:ci machine works
PASS the use is attributed to the token (last_used_at)
PASS restore the personal policy (the machine loses tag:ci)
PASS without the tag the token may not exec there
PASS revoke the tag:ci token
RESULT PASS
```

First attempt of runs 2 and 3 failed at "create a token": the holder/creator session there was the API-token-limited one I had used to sign in the new clients; redone with browser-approved sessions as above (the failure is expected behaviour, not a product bug).
