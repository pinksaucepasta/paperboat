# Control-Plane Credential Rotation

This runbook covers Paperboat mint signing keys and the revocation snapshots consumed by
`paperboat-edge` and helpers. Never paste private key material into tickets, logs, API
requests, or the workspace.

## Planned rotation

1. Generate the new Ed25519 key through the approved secret-management path.
2. Add the new key to the configured mint secret set while retaining the previous key, set
   the configured active key ID to the new ID, and deploy `paperboat-server`.
3. Confirm `/.well-known/jwks.json` contains both key IDs and new credentials use the new
   ID. Do not remove the prior public key during the overlap window.
4. Confirm tunnel diagnostics remain ready and revocation refresh errors stay at zero for
   at least the maximum issued credential lifetime plus clock skew.
5. Remove the retired private key from server signing configuration. Retain its public key
   until every credential it signed has expired.

## Emergency revocation

1. Stop issuing with the affected key by activating an uncompromised configured key and
   deploying the server.
2. As an admin operator with a current operator session and CSRF token, send
   `POST /v1/admin/mint/signing-keys/{key_id}/revoke` with `Operator-Target` (the key ID),
   an opaque `Operator-Reason`, an `Idempotency-Key` header and
   JSON body `{"reason":"<bounded operator reason>"}`; success is 204.
3. Verify `GET /v1/trust/revocations` through the private edge-control identity contains
   the key ID in `key_ids`. Do not expose the edge-control credential in command history.
4. Confirm tunnel instances refresh within one control interval. New use fails closed after
   three missed intervals when revocation state cannot be refreshed.
5. Search redacted audit events for `mint.signing_key_revoked`, correlating the request and
   operation IDs. Verify no private key, token, or credential body appears in the event.

## After a server breach: break-glass

Admin panel, Security, Break glass (`internal/breakglass`). It rotates what the server can rotate in one staged run
and signs everyone out. Keys held in server configuration are deployed ahead, by the steps above and below; the
preview refuses to start until they are, and says which are missing:

- mint signing keys: the new key as the active one beside the old one (Planned rotation, steps 1-3);
- `PAPERBOAT_SESSION_KEYS`: the new key first, the old one kept;
- `PAPERBOAT_ENCRYPTION_KEY`: a ring, `new,old`. New values are sealed under the first key and every stored
  ciphertext names its key; any key of the ring opens one.

Confirmation is the phrase `BREAK GLASS`, a reason and a current authenticator code, all checked when the run
starts. The stages, in order, each audited (`breakglass.stage_started`, `breakglass.stage_finished`):

1. publish: a pending SSH CA is added beside the active one for every account (managed SSH `AdvanceUserCA`);
2. pickup: wait until 90% of the machines that fetched their CAs recently fetched them again, or 20 minutes
   (`break_glass` in the server configuration); the panel lists each machine. Offline machines fetch on reconnect;
3. switch: the pending CAs sign, the old ones become retiring and stay trusted (certificates still verify);
4. revoke: every browser session, CLI session and token, API token, recipe, device grant, machine access session and
   other operator session ends, except the operator who started the run;
5. reseal: every stored ciphertext moves to the newest encryption key (rows no key opens are counted, not fatal);
6. floor (optional): the client update floor is raised;
7. retire: the old CAs are removed at once, old mint keys are withdrawn (`mint_withdrawn_keys`), and the stage ends
   when machines fetched the CA set without them.

The run is one database row advanced by a worker, so a crash resumes it. When it is done, remove the old mint, session
and encryption keys from the server configuration and deploy, and rotate the third-party credentials the panel lists.

## Recovery

- If the server is unavailable, restore it before changing consumer trust files. Consumers
  retain the last valid snapshot only until the freshness deadline and then reject new
  credentials.
- If a malformed snapshot is served, stop the faulty artifact, restore the last compatible
  server artifact, and verify a valid snapshot refresh. Never hand-edit a snapshot to remove
  a revocation.
- If the wrong key was revoked, do not delete or roll back the revocation record. Introduce
  a new key ID and rotate forward; key IDs are never reused.
- Rollback may restore an older server binary only if it can read the additive revocation
  table and continues serving all recorded revocations.

Record deployment IDs, affected key IDs, first/last observed refresh times, audit event IDs,
and recovery actions in the incident record. Do not record secret values.
