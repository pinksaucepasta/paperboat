# Break-glass staging drill, 2026-11-18

Build: server dev-1e2d34558 (stamped), arm64, on the anvit staging host. Isolated stack: its own database `bgdrill` in the
staging PostgreSQL container, its own server instance (port 18193, tailnet port 10001), operator with an enrolled
authenticator, one fixture account, two disposable systemd containers `bgdrill-a` and `bgdrill-b` (pb dev-1e2d34558, paired
over the API, direct path between them). The shared `pb-admin-stg` stack was not used for the run (other lanes' fixtures live in it).
No tunnel check: this stack has no edge the containers can reach, so exec and ssh are covered, tunnels are not.

## Before
- Next keys deployed beside the old ones (mint `bgnew` active beside `bgold`, new session key first, encryption key ring `new,old`).
  With only the old keys the preview showed mint and session checks red and the start was refused.
- Baseline from `bgdrill-a` as the signed-in user: `pb exec bgdrill-b -- hostname` 3.6 s, `pb ssh pbt@bgdrill-b -- hostname` 1.6 s.
- Preview: 1 account with an SSH CA, 2 machines (2 expected to pick up), 12 sessions and tokens to revoke, 1 other operator session,
  8 sealed rows to re-seal, mint keys to withdraw [bgold], 2 encryption keys, 2 session keys.

## Run (started 17:50:29 server time, TOTP code checked at the start)
| Stage | Time since start | Result |
|---|---|---|
| publish | 1.1 s to 3.2 s | 1 pending CA added |
| pickup | done at 5.3 s | 2 of 2 online machines fetched the set after their account's pending CA was created (a wake after each CA change makes hosts fetch at once) |
| switch | 7.3 s to 9.3 s | 1 account switched; new CA signs, old CA retiring |
| revoke | 11.3 s to 13.3 s | 1 API token, 3 CLI sessions, 3 CLI access tokens, 3 CLI refresh tokens revoked (the acting operator's session kept, other operators' ended) |
| reseal | 15.4 s to 17.4 s | 8 rows moved to the newest encryption key, 0 undecryptable |
| floor | skipped | not requested |
| retire | 19.4 s to 3900.7 s | old CA removed at 17:50:48; machines fetched the set without it; old mint key `bgold` withdrawn at 18:55:30 = start + 65 min (the mint drain) |
| run | 3902.7 s | done; audit: breakglass.started 1, stage_started 6, stage_finished 6, finished 1 |

## After
- Old session (the API token used before): `pb exec` and `pb ssh` refused in 0.2 s, "Your Paperboat session is no longer valid" (also in the
  probe loop from the revoke stage on).
- New API token: `pb login --api-token -`, then `pb exec bgdrill-b -- hostname` 4.4 s and `pb ssh pbt@bgdrill-b -- hostname` 0.8 s, **after the old
  mint key was withdrawn**. No reinstall, nothing changed on the machines. Both machines `online` with fresh `last_seen_at`; 4 identity renewals during the drain.
- `/.well-known/jwks.json`: kids [bgnew], withdrawn_key_ids [bgold]. One CA left, active.

## Findings of earlier drill runs on this stack (all fixed before this run, tests added)
1. Pickup waited for the hosts' slow timer: a machine woken at publish fetched at once, but that fetch was older than the stage's finish time, so
   it did not count. Fixed: a machine counts when it fetched after its own account's pending CA was created; retire's removal time is taken before the wake.
2. Withdrawing the old mint key at retire left both machines **offline** (they hold identity credentials signed by it, up to an hour old, and the
   server refused them until they renewed). Fixed: the old mint keys are withdrawn `mint_drain` (65 min) after the run starts.
3. The server does not migrate on start (`paperboat-server migrate`), and a worker that cannot find the break-glass tables logs an error every 2 s until it is migrated.

## Admin panel click-through (real browser, 2026-11-19, build dev-3a7a6446b, screenshots in breakglass-ui-20261118/)
Isolated stack again (own database and server, admin panel build pointed at it, 2 machines). Signed in as a throwaway operator with an enrolled authenticator.
1. `1-keys-not-deployed-checks-blocked.png`: with only the old keys deployed the preview shows the mint and session checks Blocked.
2. `2-start-disabled-third-party-list.png`: the Break glass button is disabled ("Deploy the missing keys above first"); the third-party checklist with provider links is shown.
3. `3-confirmation-dialog.png`: after deploying the next keys all checks are Ready; the dialog asks for a reason, a current authenticator code, an optional update floor, the audit reference and the typed phrase; the button stays disabled until the phrase and reference are filled.
4. `4-wrong-authenticator-code-refused.png`: a wrong code is refused ("wrong, expired or already used") with a support reference; no run was created.
5. `5-live-progress-pickup-1-of-2.png`: a run with machine b stopped: publish done, pickup running, "1 of 2 online machines hold the new keys" with the machine list (pending first).
6. `6-run-cancelled.png`: Cancel run from the panel; the run shows Cancelled (state `cancelled` in the database).
7. `7-full-run-retire-mint-countdown.png`: a second run with both machines online: publish, pickup, switch, revoke (counts), reseal done in seconds; retire running with "Old mint keys are withdrawn in 1 hour (machines renew their credentials first)". The operator's own panel session stayed signed in through the revoke stage.
The panel's pairing-token revocation was added after the drill of 2026-11-18 (revoke stage now also cancels unused enrollment tokens and unredeemed install material).
Fixes made after looking at the screenshots: ready checks no longer show the "deploy ..." instruction, "1 CA" singular, a cancelled run shows its running stage as stopped and hides the live machine list.
