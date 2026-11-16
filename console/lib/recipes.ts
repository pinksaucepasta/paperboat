"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, ApiError, idempotencyKey } from "@/lib/api"
import { queryKeys } from "@/lib/query-keys"
import type { NewToken } from "@/lib/recipe-token"

// Recipes. Contract: shared/recipe/README.md ("API"). `{ws}` is "personal" or the team id.

export type Recipe = {
  id: string
  name: string
  revision: number
  /** `pbr_<id>_…`: the secret is never shown again. */
  token: string
  source: string
  format: "toml" | "json"
  /** The canonical file, defaults filled. */
  recipe: { tags?: string[]; env?: { vaults: string[] } }
  created_by: string
  created_at: string
  updated_at: string
  expires_at: string
  last_used_at: string | null
  rotated_at: string | null
  machines: number
  max_machines?: number
}

export type RecipeMachine = {
  id: string
  name: string
  online: boolean
  last_seen_at: string | null
  applied_revision: number
  /** What each section did on the machine; a section that failed carries `error`. */
  apply_state: Record<string, { error?: string } | unknown> | null
  remove_at: string | null
}

/** One problem in the file, as `invalid_recipe` lists them. */
export type FileProblem = { line: number; column: number; path: string; message: string; text: string }

export type Impact = { machines: { id: string; name: string; online: boolean }[]; vaults: string[] }

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
    queryFn: async ({ signal }) => (await api<{ recipes: Recipe[] }>(`${ws(workspace)}/recipes`, { signal })).recipes,
    enabled: Boolean(workspace),
  })

export const recipeQuery = (workspace: string, name: string) =>
  queryOptions({
    queryKey: queryKeys.recipes(workspace, name),
    queryFn: ({ signal }) =>
      api<{ recipe: Recipe; machines: RecipeMachine[] }>(recipePath(workspace, name), { signal }),
    enabled: Boolean(workspace),
  })

export const createRecipe = (workspace: string, file: { name: string; source: string; format: RecipeFormat }, token: NewToken) =>
  write<{ recipe: Recipe }>(`${ws(workspace)}/recipes`, "POST", { ...file, token_id: token.id, auth: token.auth })

export const updateRecipe = (workspace: string, name: string, file: { source: string; format: RecipeFormat }) =>
  write<{ recipe: Recipe; previous: Recipe; online_machines: number }>(recipePath(workspace, name), "PUT", file)

/** Rotate and revoke refuse (409) until sent the current code; the refusal carries it and the impact. */
export const rotateRecipe = (workspace: string, name: string, token: NewToken, confirm: string) =>
  write<{ recipe: Recipe }>(`${recipePath(workspace, name)}/rotate`, "POST", { auth: token.auth, confirm })

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

/** The recipe reads vaults but this browser cannot wrap their keys to its token yet. */
export const needsVaultKey = (error: unknown) => error instanceof ApiError && error.code === "vault_key_missing"

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
