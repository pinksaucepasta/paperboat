# Team lifecycle and activity

Paperboat team roles govern administration. Resource use still requires an explicit grant,
and roles never provide ENV decryption keys by themselves. The owner appoints or removes
admins, transfers ownership, and deletes the team. Admins manage ordinary membership and
resource grants within the current team policy.

An owner must transfer ownership before leaving. Removing a member withdraws that person's
active team access immediately. Their personal resources remain theirs, while personal
resources they shared with the team are withdrawn. A machine already transferred into team
ownership stays with the team when its original enroller leaves. Deleting the team revokes
team-owned devices instead of converting them into personal devices. Team deletion also
removes the team's encrypted receipt-SMTP configuration.

Pending, approved, or consumed Team Inbox requests are revoked when team membership, the
machine binding, or the required file capability is removed. Revocation prevents new or
resumed authorization. It cannot erase file bytes that a recipient already received.
A surviving team-owned machine keeps its recorded original enroller identity. If that
enroller leaves, Paperboat does not redirect Inbox delivery to a new owner or another member:
new requests are denied and pending, approved, or consumed authorization is revoked.

Removing a member revokes their adopted team config assignment and deletes that adoption.
Rejoining the team does not revive it; the member must explicitly adopt the current default
again. Paperboat membership controls the team default and its adoption. Access granted
directly by a Git provider remains external and is not revoked or represented as revoked by
Paperboat.

Team ENV status appears in `pb team get` and the dashboard:

- `not_initialized`: the team has no ENV epoch yet.
- `ready`: the current epoch is available through authorized key delivery.
- `rotation_pending`: revocation has been applied, but rotation cannot finish until an
  authorized owner or admin supplies access to the current key material.
- `deleted`: the team and its ENV are no longer usable.

Rotation never bypasses the key requirement. Owners and admins can coordinate rotation only
when they already have authorized key access; their role does not decrypt the ENV. Revocation
cannot make someone forget secrets they already learned, just as it cannot erase files already
received. Rotate any externally usable credentials that a departing member may know.

Shared terminal access records two separate administrative events. Access issuance records
`terminal_session_access_authorized`; the target runtime records
`terminal_session_joined` only after attaching the actual participant and after the server
rechecks the current access and team generations. The runtime persists that join before it
returns the attach response or replays output. If durable recording fails, it detaches the
new participant and asks the client to retry. These events contain attachment and
authorization metadata only, never terminal output or input.

Current owners and admins can inspect bounded administrative metadata with:

```text
pb team activity TEAM [--limit N] [--cursor CURSOR] [--json]
```

The default page size is 50 and the maximum is 200. Pass the opaque `next_cursor` into
`--cursor` for the next page. Authorization is checked again for every page, so removal or
team deletion stops later reads. Activity is retained for 90 days and physically purged in
up to 200-row batches once per minute. A transient cleanup failure is retried on the next
interval without stopping other workers. Actions omit the internal `team.` prefix. Activity excludes terminal
contents, keystrokes, file contents, filenames, file digests, manifests, and ENV secrets.
The dashboard uses the same owner/admin-only activity feed and displays pending ENV status
with the required recovery action.
