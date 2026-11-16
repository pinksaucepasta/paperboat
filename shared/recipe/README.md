# Recipes: file, token and API (PLAN F26)

`shared/recipe` is the one owner of the recipe file (schema, validation, did-you-mean, canonical
JSON) and the token format, for the CLI, the server and, through the server's errors, the
console. The server parses the source it is sent with this same package.

## Token

`pbr_<id>_<secret>`: `id` 10 and `secret` 52 lowercase base32 characters (32 random bytes), 67
characters in all. The creator's computer makes it (`recipe.NewToken`); the server never sees the
secret. `Token.Auth()` (HKDF-SHA256 "paperboat recipe auth") is sent to the server, which stores
`sha256(auth)`; `Token.PrivateKey()` (HKDF "paperboat recipe key", X25519) never leaves the
device, and vault keys are wrapped to `Token.PublicKey()`. Rotate keeps the `id` and changes the
secret.

## API

Byte fields (`auth`, `wrapped`, `sealed`, `envelope`) are standard base64. `{ws}` is `personal`
or the team id. Errors use the server envelope `{"error": {"code", "message", "details"}}`.

| Method and path | Who | Request | Response |
|---|---|---|---|
| `POST /v1/workspaces/{ws}/recipes` | personal: owner; team: owners, admins | `{name, source, format: "toml"\|"json", token_id, auth, vault_wraps?: [{vault, wrapped}]}` | 201 `{recipe}`; same `token_id`+`auth` again returns the same recipe |
| `GET /v1/workspaces/{ws}/recipes` | members | — | `{recipes: [Recipe]}` |
| `GET /v1/workspaces/{ws}/recipes/{name}` | members | — | `{recipe, machines: [Machine]}` |
| `PUT /v1/workspaces/{ws}/recipes/{name}` | as create | `{source, format, vault_wraps?}` | `{recipe, previous, online_machines}` |
| `POST /v1/workspaces/{ws}/recipes/{name}/rotate` | as create | `{auth, confirm, machine_keys?: [{machine_id, sealed}]}` | `{recipe}` |
| `POST /v1/workspaces/{ws}/recipes/{name}/revoke` | as create | `{confirm}` | `{recipe, impact}` |
| `GET /v1/workspaces/{ws}/env-vaults` | members | — | `{vaults: [Vault]}` (no envelope) |
| `POST /v1/workspaces/{ws}/env-vaults` | as create | `{name, wraps: [{recipient_kind: "account"\|"team", recipient_id, wrapped}]}` | 201 `{vault}` |
| `GET /v1/workspaces/{ws}/env-vaults/{name}` | members | — | `{vault}` with `envelope` and the caller's `wraps` |
| `PUT /v1/workspaces/{ws}/env-vaults/{name}/values` | as create | `{expected_revision, key_epoch, envelope}` | `{vault}` |
| `POST /v1/workspaces/{ws}/env-vaults/{name}/rekey` | as create | `{expected_key_epoch, envelope, wraps, recipe_wraps: [{recipe, wrapped}]}` | `{vault}`; recipes not listed lose access |
| `DELETE /v1/workspaces/{ws}/env-vaults/{name}` | as create | — | 204 |
| `POST /v1/auth/recipe-sessions` | anyone with the token | `{token_id, auth, client_label, device_type: "desktop"\|"server"\|"container", os}` | CLI token set + `recipe: {id, name, revision, workspace, recipe}` |
| `GET /v1/recipe-machine` | a recipe session (bearer) | — | `{id, name, revision, recipe, vaults: [{name, key_epoch, revision, envelope, wrapped}], machine_key?}` |
| `PUT /v1/recipe-machine/apply` | a paired recipe session | `{revision, sections: {env\|config\|preview\|tunnel: any}}` | 204 |

```text
Recipe  {id, name, workspace, revision, token ("pbr_<id>_…"), source, format, recipe (canonical),
         created_by ("user:<email>"), created_at, updated_at, expires_at, last_used_at, rotated_at,
         machines, max_machines?}
Machine {id, name, online, last_seen_at, applied_revision, apply_state, remove_at}
Vault   {name, key_epoch, revision, rekey_required, envelope?, wraps, recipes}
impact  {machines: [{id, name, online}], vaults: [name]}
```

Pairing from a recipe: `POST /v1/auth/recipe-sessions`, then `POST /v1/machines/setup` with that
session's bearer (as `pb setup` today). Setup makes the machine the recipe's in one transaction:
counted against `max_machines`, shared into a team recipe's team, tagged with the recipe's tags
(one policy version authored by the recipe), given its `machine_lifetime` deadline, and bound to
the session (the reach never limits the machine's own pb).

A recipe session acts as the creator in the recipe's workspace only (another `workspace` is 403).
Its writes are limited to its own machine (`/v1/machines/<own>/…`, `/v1/environment/hosts/<own>/…`),
setup, `/v1/recipe-machine…`, and machine actions whose authorization checks the reach (exec,
terminal, files, private access descriptors); anything else is 403 `outside_recipe_reach`. A
machine action outside `[[reach]]` is 403 `outside_recipe_reach`.

Errors:

| Code | Status | When; details |
|---|---|---|
| `invalid_recipe` | 422 | the file; `errors: [{line, column, path, message, text}]` (`text` is `file:line:col: message`) |
| `invalid_recipe_reach` | 422 | a `[[reach]]` name the workspace policy does not have; `errors` (policy errors) |
| `unknown_vault`, `unknown_repository` | 422 | `{kind, name, known, suggestion}` |
| `vault_key_missing` | 422 | the recipe newly reads a vault and no `vault_wraps` entry carries its key; `{vault}` |
| `recipe_name_taken` | 409 | another live recipe has the name |
| `confirmation_required` | 409 | rotate/revoke without the current code; `{confirm, impact}` |
| `forbidden` | 403 | a team member who is not an owner or admin |
| `recipe_invalid` | 401 | unknown token, wrong auth, or revoked |
| `recipe_expired` | 401 | `{recipe, at}` |
| `recipe_replaced` | 401 | an older token after rotate; `{recipe, at}` |
| `recipe_full` | 409 | `{recipe, max_machines}` (session start and setup) |
| `rotation_required` | 409 | vault values written after a recipe that read the vault was revoked: rekey first |
| `vault_in_use` | 409 | deleting a vault a recipe reads |

Machines leave a recipe through the `pb machine revoke` path: revoke removes all of them;
`machine_lifetime` removes each when it ends; `remove_after_offline` removes a machine that has
been offline that long (cancelled while it is online; the server checks every minute).
