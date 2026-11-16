"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, ApiError, idempotencyKey, type Data, type Schemas } from "@/lib/api"
import { queryKeys } from "@/lib/query-keys"
import { tokenId, type NewToken } from "@/lib/recipe-token"
import { withVaultKeys, type VaultWrap } from "@/lib/recipe-vaults"

// Recipes. Contract: shared/recipe/README.md ("API"). `{ws}` is "personal" or the team id.

/** `token` is `pbr_<id>_…` (the secret is never shown again); `recipe` is the canonical file, defaults filled. */
export type Recipe = Schemas["RecipeView"]
/** What each section did on the machine (`apply_state`); a section that failed carries `error`. */
export type RecipeMachine = Schemas["RecipeMachine"]

/** One problem in the file, as `invalid_recipe` lists them. */
export type FileProblem = { line: number; column: number; path: string; message: string; text: string }

export type Impact = Schemas["RecipeImpact"]

export type RecipeFormat = Recipe["format"]

const ws = (workspace: string) => `workspaces/${encodeURIComponent(workspace)}`
const recipePath = (workspace: string, name: string) => `${ws(workspace)}/recipes/${encodeURIComponent(name)}`
const write = <T>(path: string, method: "POST" | "PUT", body: unknown) =>
  api<T>(path, {
    method,
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify(body),
  })

export const recipesQuery = (workspace: string) =>
  queryOptions({
    queryKey: queryKeys.recipes(workspace),
    queryFn: async ({ signal }) => (await api<Data<"/v1/workspaces/{ws}/recipes">>(`${ws(workspace)}/recipes`, { signal })).recipes,
    enabled: Boolean(workspace),
  })

export const recipeQuery = (workspace: string, name: string) =>
  queryOptions({
    queryKey: queryKeys.recipes(workspace, name),
    queryFn: ({ signal }) =>
      api<Data<"/v1/workspaces/{ws}/recipes/{name}">>(recipePath(workspace, name), { signal }),
    enabled: Boolean(workspace),
  })

/** Creates the recipe; a vault it reads is wrapped to its token here (ENV unlocked), as `pb recipe create` does. */
export const createRecipe = (workspace: string, file: { name: string; source: string; format: RecipeFormat }, token: NewToken) =>
  withVaultKeys(workspace, token, (vault_wraps) =>
    write<Data<"/v1/workspaces/{ws}/recipes", "post">>(`${ws(workspace)}/recipes`, "POST", {
      ...file,
      token_id: token.id,
      auth: token.auth,
      public_key: token.publicKey,
      vault_wraps,
    }),
  )

/** The token id and public key of an existing recipe, for wrapping vault keys to it; undefined before vault keys. */
export const recipeKey = (recipe: Recipe) => {
  const id = tokenId(recipe.token)
  return id && recipe.public_key ? { id, publicKey: recipe.public_key } : undefined
}

export async function updateRecipe(workspace: string, recipe: Recipe, file: { source: string; format: RecipeFormat }, wraps: VaultWrap[] = []) {
  const send = (vault_wraps: VaultWrap[]) =>
    write<Data<"/v1/workspaces/{ws}/recipes/{name}", "put">>(recipePath(workspace, recipe.name), "PUT", { ...file, vault_wraps })
  const key = recipeKey(recipe)
  // A recipe made before vault keys has no public key to wrap to: it can still change when it needs no new vault.
  return key ? withVaultKeys(workspace, key, send, wraps) : send(wraps)
}

/**
 * Rotate and revoke refuse (409) until sent the current code; the refusal carries it and the impact. A rotate of a
 * recipe that reads vaults moves its vault keys to the new token's public key in the same call (ENV unlocked).
 */
export const rotateRecipe = (workspace: string, name: string, token: NewToken, confirm: string) =>
  withVaultKeys(workspace, token, (vault_wraps) =>
    write<Data<"/v1/workspaces/{ws}/recipes/{name}/rotate", "post">>(`${recipePath(workspace, name)}/rotate`, "POST", {
      auth: token.auth,
      public_key: token.publicKey,
      vault_wraps,
      confirm,
    }),
  )

export const revokeRecipe = (workspace: string, name: string, confirm: string) =>
  write<{ recipe: Recipe; impact: Impact }>(`${recipePath(workspace, name)}/revoke`, "POST", { confirm })

/** The file's problems when the server refused it; none for any other error. */
export const fileProblems = (error: unknown): FileProblem[] =>
  error instanceof ApiError && error.code === "invalid_recipe"
    ? ((error.details as { errors?: FileProblem[] } | undefined)?.errors ?? [])
    : []

/** The code and impact a rotate or revoke must be confirmed with. */
export const confirmation = (error: unknown): { confirm: string; impact: Impact } | undefined =>
  error instanceof ApiError && error.code === "confirmation_required"
    ? (error.details as { confirm: string; impact: Impact })
    : undefined

/** What each machine says about the recipe's sections, for the sections that failed. */
export function failedSections(machine: RecipeMachine): string[] {
  return Object.entries(machine.apply_state ?? {}).flatMap(([section, state]) => {
    const error = (state as { error?: unknown } | null)?.error
    return typeof error === "string" ? [`${section}: ${error}`] : []
  })
}

/** `toml` or `json` for a file name or, for pasted text, by its first character. */
export function formatOf(text: string, fileName?: string): RecipeFormat {
  if (fileName?.toLowerCase().endsWith(".json")) return "json"
  if (fileName?.toLowerCase().endsWith(".toml")) return "toml"
  return text.trimStart().startsWith("{") ? "json" : "toml"
}

/** A recipe name from a file name: `ci.toml` is `ci`. */
export const nameFromFile = (fileName: string) => fileName.replace(/\.[^.]*$/, "").toLowerCase()

export const nameHint = "Lowercase letters, numbers and dashes. Up to 63."
export const validName = (name: string) => /^[a-z0-9][a-z0-9-]{0,62}$/.test(name)

/** Starting points for the empty list: PLAN F26 "Three recipes". */
export const templates = [
  {
    id: "ci",
    title: "CI runner",
    about: "Tests with ENV and a preview",
    name: "ci",
    source: `# A CI runner: secrets for the tests, a 2-hour preview of the app.
tags = ["ci"]
max_machines = 20
remove_after_offline = "30m"

[env]
vaults = ["canary-env"]

[[preview]]
port = 3000
ttl = "2h"
`,
  },
  {
    id: "homelab",
    title: "Homelab fleet",
    about: "Config from a repository, a tunnel per machine",
    name: "homelab",
    source: `# Every VM made from this image pulls dotfiles and publishes its dashboard.
tags = ["homelab"]
expires = "365d"
remove_after_offline = "7d"

[config]
repository = "dotfiles"

[[tunnel]]
port = 8123
private = true
`,
  },
  {
    id: "agent",
    title: "Agent sandbox",
    about: "May run commands only on tag:ci",
    name: "agent",
    source: `# A disposable sandbox for a coding agent: it can run commands and copy files on CI machines, nothing else.
max_machines = 1
remove_after_offline = "10m"

[[reach]]
dst = ["tag:ci"]
allow = ["exec", "managed_ssh", "files"]
`,
  },
] as const
