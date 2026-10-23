# Workspace policy reference

One policy per workspace (personal or team) says who may use what on which machines. It is a
JSON file; comments (`//`, `/* */`) and trailing commas are allowed (HuJSON). The JSON Schema is
[`schema.json`](schema.json), served at `GET /v1/policy/schema` and printed by
`pb policy schema`. Design: PLAN.md F18.

```jsonc
{
  // Team workspaces only; false (the default) means no rule gives private_access.
  "allow_private_access": true,
  "groups": {"group:oncall": ["user:cy@example.test"]},
  "tags": {
    "tag:prod": ["machine:db1", "machine:web2"],
    "tag:ci": ["machine:runner"],
  },
  "rules": [
    {"src": ["role:member"], "dst": ["tag:ci"], "allow": ["terminal", "exec", "files"]},
    {"src": ["group:oncall"], "dst": ["tag:prod"], "allow": ["*"]},
    {"src": ["*"], "dst": ["tag:prod"], "deny": ["managed_ssh"], "note": "no SSH on prod"},
    {"src": ["group:oncall"], "dst": ["tag:prod"], "allow": ["private_access"], "ports": [5432]},
    {"src": ["user:bo@example.test"], "dst": ["preview:p1"], "allow": ["inspect"]},
    {"src": ["group:oncall"], "dst": ["env:team"], "allow": ["env_read"]},
  ],
  "tests": [
    {"src": "user:cy@example.test", "dst": "machine:db1", "allow": ["exec", "private_access:5432"], "deny": ["managed_ssh"]},
    {"src": "user:bo@example.test", "dst": "machine:db1", "deny": ["terminal"]},
  ],
}
```

## How a request is decided

1. **Guardrails** outside the policy: signed in, account active, team not suspended, member
   active, machine enrolled and in this workspace, the machine's own service switches
   (`pb machine capabilities`). Any of them failing denies.
2. **Owner:** the owner of a personal machine may do everything on it in the personal
   workspace, and the owner of a preview or tunnel (or of the personal machine it runs on) may
   always `inspect` and `replay` it. No rule can take this away.
3. **Deny:** a deny rule that matches decides: denied.
4. **Allow:** an allow rule that matches decides: allowed.
5. Otherwise denied.

Rule order never matters. Explain (`pb policy explain`) names the rule that decided.

**API tokens** act as the member who created them. A token created with a scope
(`[{"dst": ["tag:ci"], "allow": ["exec"]}]`, the same names as a rule) may only do what the
scope lists, and never more than its creator may do at that moment.

## Fields

| Field | Meaning |
|---|---|
| `allow_private_access` | Team only. When false (the default), no rule gives `private_access`. |
| `groups` | `group:<name>` → list of `user:<email>`. |
| `tags` | `tag:<name>` → list of `machine:<alias>`. A machine may carry many tags. |
| `rules[]` | `src`, `dst`, exactly one of `allow` / `deny`, optional `ports`, optional `note`. |
| `tests[]` | `src` (one `user:<email>`), `dst` (one machine, preview, tunnel or `env:team`), `allow` and/or `deny` lists of actions; `private_access:<port>` checks one port. A failing test stops the save. |

Names after `group:` and `tag:` use lowercase letters, digits, `.`, `_` and `-` (up to 63).

**Subjects (`src`):** `user:<email>`, `group:<name>`, `role:owner`, `role:admin` (owners and
admins), `role:billing` and `role:auditor` (exactly that role), `role:member` (every member),
`*` (every member). Roles are admin rights only; machine use comes from rules.

**Targets (`dst`):** `machine:<alias>` (or `machine:<owner email>/<alias>` when two machines
shared into a team have the same alias), `machine:*` (every machine of the workspace),
`tag:<name>`, `preview:<name>`, `tunnel:<name>`, `env:team` (team ENV). One rule's targets
are all machines/tags, all previews/tunnels, or `env:team`.

**Actions:**

| Action | Targets | Meaning |
|---|---|---|
| `terminal` | machines | open terminals, including the browser terminal |
| `exec` | machines | `pb exec` |
| `managed_ssh` | machines | `pb ssh`, `scp`, `sftp`, `rsync` |
| `files` | machines | send files to the machine's Inbox |
| `preview_manage` | machines | start and stop previews on the machine |
| `tunnel_manage` | machines | create and change tunnels on the machine |
| `private_access` | machines | reach the machine's ports privately; `ports` narrows it (an allow defaults to 1024-65535, a deny to every port) |
| `inspect` | machines, previews, tunnels | read captured requests (on a machine: every preview and tunnel it publishes) |
| `replay` | machines, previews, tunnels | replay a captured request; `inspect` does not imply it |
| `*` | machines | all of the above |
| `env_read`, `env_write` | `env:team` | receive or change the team ENV |

## Error codes

Every error has a JSON Pointer `path` into the file, a stable `code` and a `message`. Errors
are sorted by path, so the same file always gives the same list.

| Code | Meaning |
|---|---|
| `syntax` | not JSON / HuJSON |
| `too_large` | the file is larger than the limit |
| `wrong_type` | a value has the wrong type |
| `unknown_field` | a key the format does not have |
| `duplicate_key` | a key appears twice in one object |
| `required` | a required key is missing |
| `allow_or_deny` | a rule needs exactly one of `allow` and `deny` |
| `empty` | a list must have at least one entry |
| `limit` | more rules, tags, groups, tests or entries than the limit |
| `invalid_name` | a group or tag name is not `group:<name>` / `tag:<name>` |
| `invalid_subject` | not a valid `src` entry (or a group entry that is not `user:`) |
| `invalid_target` | not a valid `dst` entry |
| `invalid_port` | a port outside 1-65535 or a bad range |
| `unknown_user` | the email is not a member of this workspace |
| `unknown_group` | the group is not defined in `groups` |
| `unknown_role` | not `role:owner`, `role:admin`, `role:billing`, `role:auditor` or `role:member` |
| `unknown_machine` | no machine with that alias in this workspace |
| `ambiguous_machine` | two machines of the team have that alias; use `machine:<owner email>/<alias>` |
| `unknown_tag` | the tag is not defined in `tags` |
| `unknown_preview`, `unknown_tunnel` | no preview / tunnel with that name in this workspace |
| `unknown_env_scope` | the ENV target is `env:team` |
| `not_allowed_in_tag` | a tag lists something other than `machine:<alias>` |
| `team_only` | `allow_private_access` or `env:team` in a personal workspace |
| `mixed_targets` | one rule mixes machines, previews/tunnels and `env:team` |
| `unknown_action` | not an action |
| `action_not_for_target` | the action does not apply to the rule's targets |
| `ports_need_private_access` | `ports` on a rule whose actions are not exactly `private_access` |
| `unknown_subject` | a test's `src` is not a member |
| `unknown_target` | a test's `dst` is not in this workspace |
| `test_failed` | a test's expectation does not hold; the message says what the policy decides and why |

## HTTP API

`{ws}` is `personal` or the team slug. Every endpoint needs a signed-in session (CLI device
session, console session or API token) whose account is an active member of `{ws}`. Errors use
the server's envelope `{"error": {"code", "message", "details"}}`; policy errors are in
`details.errors` as the list above. Clients ignore unknown response fields.

| Method and path | Who | Request | Response |
|---|---|---|---|
| `GET /v1/policy/schema` | anyone | — | the JSON Schema |
| `GET /v1/workspaces/{ws}/policy[?version=N]` | members | — | `Policy` |
| `POST /v1/workspaces/{ws}/policy/validate` | members | `{"policy": "<file text>"}` | `Validation` |
| `PUT /v1/workspaces/{ws}/policy` | editors | `{"policy", "expected_version", "message"?, "confirm"?}` | `Policy` |
| `POST /v1/workspaces/{ws}/policy/explain` | members (own access), owners, admins and auditors (anyone) | `{"policy"?, "src"?, "dst"?, "action"?, "port"?, "api_token"?}` | `Explanation` |
| `GET /v1/workspaces/{ws}/policy/versions` | members | — | `{"versions": [Version]}` newest first |
| `POST /v1/workspaces/{ws}/policy/restore` | editors | `{"version", "expected_version", "message"?, "confirm"?}` | `Policy` |

```text
Policy      {workspace, version, policy (file text, comments kept), digest ("sha256:<hex>"),
             updated_at, updated_by ("user:<email>"), updated_with_api_token? (name), message,
             can_edit ("all" | "own_machines" | "none"),
             names {users: [email], machines: [alias], previews: [name], tunnels: [name],
                    tags: [tag], groups: [group]}}
Validation  {valid, errors: [Error], tests: {passed, failed},
             access_changes: [Change] (against the saved version), confirm ("sha256:<hex>")}
Explanation {decision?: {allowed, reason, rules: ["/rules/N"], guardrail?,    // src+dst+action
                         key_delivered?},  // false: env:team allowed, team key not delivered yet
             who_can?: [Access],                                              // dst+action
             what_can?: [Access]}                                             // src only
Access      {subject, target, action, ports?, reason ("owner" | "rule"), rules?}
Change      {change ("gained" | "lost"), subject, target, action, ports?, rules?}
Version     {version, digest, created_at, author, api_token?, message, gained, lost,
             kind ("saved" | "restored" | "rewritten" | "migrated")}
```

- **Saving** (PUT, restore): the file is validated first (422 `invalid_policy`, errors in
  `details.errors`). A stale `expected_version` is 409 `policy_version_conflict`. When the save
  changes anyone's access and `confirm` is missing or not the digest of that change list, it is
  412 `policy_changes_unconfirmed` with `details.access_changes` and `details.confirm`; send the
  same request again with that `confirm`. A save that changes no access needs no `confirm`.
- **Editors**: owners and admins (`can_edit: "all"`); a member who owns a personal machine
  shared into the team may save a change that only adds, removes or changes rules whose `dst`
  is only their own machines (`can_edit: "own_machines"`; otherwise 403
  `policy_edit_forbidden`). Personal workspace: the owner.
- **Explain** reasons: `owner`, `rule`, `denied_by_rule`, `no_rule`, `port_not_allowed`,
  `private_access_off`, `outside_token_scope`; `guardrail` names a failed guardrail
  (`team_suspended`, `membership_inactive`, `machine_not_in_workspace`, `team_not_entitled`,
  `machine_service_off`). A member asking about someone else is 403 `policy_explain_forbidden`.
- **Team ENV**: `env_read`/`env_write` on `env:team` decide who may use the team ENV (write
  covers reading); the team owner always may (owner guardrail). A team starts with
  `{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]}`.
  `pb team grant <team> <account> env <team> read|write` sets that member's own rule (one src,
  `dst` `env:team`; `--active=false` removes it), and the team view lists those rules as `env`
  grants. A save after which a member who holds the team key may no longer read the ENV takes
  the key, as `pb env team revoke` does (rotation required). A member allowed without a key yet
  is `allowed` with `key_delivered: false`: a key holder delivers it with `pb env grants sync`.

## API tokens

An API token acts as the member who created it, in one workspace, until it expires or is
revoked. An optional scope (a list of `{"dst", "allow", "ports"?}` entries using the names
above, validated like rules) narrows it; a scope never widens it.

| Method and path | Who | Request | Response |
|---|---|---|---|
| `POST /v1/workspaces/{ws}/api-tokens` | members | `{"name", "expires_in_days", "scope"?}` | `{"api_token": APIToken, "secret": "pbat_…"}` (secret shown once) |
| `GET /v1/workspaces/{ws}/api-tokens` | members (own), owners/admins (all) | — | `{"api_tokens": [APIToken]}` |
| `DELETE /v1/workspaces/{ws}/api-tokens/{id}` | its creator, owners/admins | — | `{"api_token": APIToken}` (revoked) |

```text
APIToken {id, name, workspace, created_by ("user:<email>"), created_at, expires_at,
          last_used_at?, revoked_at?, scope? ([{dst, allow, ports?}])}
```

- `name`: same characters as tag names, unique per creator and workspace (409
  `api_token_name_taken`). `expires_in_days`: 1 to the configured maximum (default 90).
  A bad scope is 422 `invalid_api_token_scope` with `details.errors`.
- Use: `Authorization: Bearer pbat_…` on any API request; the request acts as the creator in
  the token's workspace only (another workspace is 403 `workspace_unavailable`), and every
  authorization also checks the scope (`outside_token_scope`). Expired or revoked: 401
  `api_token_invalid`.
- Scoped tokens: a request made with a scoped token (directly, or through a pb session made from
  it) reads as the creator; machine actions (terminal, exec, managed SSH, files) are checked
  against the scope (`outside_token_scope`); other changes are refused, except the policy's
  explain/validate, revoking API tokens and the connection plumbing pb needs (including its own
  E2EE certificate, never a fresh enrollment that replaces the account's root). No request made
  with an API token, directly or through its session, can create an API token (403 `forbidden`).
- `POST /v1/auth/api-token-sessions` (bearer: the API token) `{"client_label", "device_type":
  "desktop|server|container", "os"}` returns a CLI token set for a session of the creator that
  carries the token; it ends when the token is revoked or expires. A session cannot start
  another (401 `api_token_required`).
