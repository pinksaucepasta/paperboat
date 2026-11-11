"use client"

import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query"
import { ApiError, confirmSession, idempotencyKey, send } from "@/lib/api"
import { queryKeys } from "@/lib/query-keys"

// Workspace policy and API tokens. Contract: shared/policy/README.md ("HTTP API", "API tokens").
// Hand-written types until the server documents these endpoints in openapi.json.

export type PolicyNames = {
  users: string[]
  machines: string[]
  previews: string[]
  tunnels: string[]
  tags: string[]
  groups: string[]
}

export type CanEdit = "all" | "own_machines" | "none"

export type Policy = {
  workspace: string
  version: number
  policy: string
  digest: string
  updated_at: string
  /** Absent when the server itself saved it ("Removed machines or members that left the workspace"). */
  updated_by?: string
  updated_with_api_token?: string
  message: string
  can_edit: CanEdit
  names: PolicyNames
}

export type PolicyProblem = { path: string; code: string; message: string }

export type AccessChange = {
  change: "gained" | "lost"
  subject: string
  target: string
  action: string
  /** private_access only, e.g. "22,1024-65535" */
  ports?: string
  rules?: string[]
}

export type Validation = {
  valid: boolean
  errors: PolicyProblem[]
  tests: { passed: number; failed: number }
  access_changes: AccessChange[]
  confirm: string
}

/** `key_delivered: false`: env_read is allowed but no key holder has delivered the team ENV key yet. */
export type Decision = {
  allowed: boolean
  reason: string
  rules?: string[]
  guardrail?: string
  key_delivered?: boolean
}
export type Explanation = { decision?: Decision }

export type PolicyVersion = {
  version: number
  digest: string
  created_at: string
  /** `user:<email>`; absent for versions nobody saved (the first one, written by Paperboat). */
  author?: string
  api_token?: string
  message: string
  gained: number
  lost: number
  kind: "saved" | "restored" | "rewritten" | "migrated"
}

export type ScopeEntry = { dst: string[]; allow: string[]; ports?: (number | string)[] }

export type APIToken = {
  id: string
  name: string
  workspace: string
  created_by: string
  created_at: string
  expires_at: string
  last_used_at?: string
  revoked_at?: string
  scope?: ScopeEntry[]
}

/** A policy error with the server's `details` (the error list, or the access changes to confirm). */
export class PolicyError extends ApiError {
  constructor(
    status: number,
    code: string,
    message: string,
    readonly details: { errors?: PolicyProblem[]; access_changes?: AccessChange[]; confirm?: string },
  ) {
    super(status, code, message)
  }
}

/** The server answers 404 (or 401 for an unknown path, NOTES.md) until the policy API is deployed. */
export const notOnServer = "policy_unavailable"

const sentences: Record<string, string> = {
  [notOnServer]: "Access policy isn't available on this server yet.",
  policy_version_conflict: "Someone saved the policy after you opened it.",
  policy_edit_forbidden: "You can change access only for your own machines.",
  policy_explain_forbidden: "Only owners and admins can test someone else's access.",
  api_token_name_taken: "You already have a token with this name.",
  forbidden: "You don't have permission to do this.",
}

async function call<T>(step: string, path: string, init?: RequestInit & { json?: unknown }): Promise<T> {
  const { json, ...rest } = init ?? {}
  const response = await send(`/api/v1/${path}`, {
    ...rest,
    ...(json !== undefined && { body: JSON.stringify(json) }),
    headers: {
      accept: "application/json",
      ...(json !== undefined && { "content-type": "application/json" }),
      ...(rest.method && rest.method !== "GET" && { "idempotency-key": idempotencyKey() }),
    },
  })
  // A 401 with a valid session is the policy API's own refusal, read below like any error.
  if (response.status === 401) await confirmSession()
  const body = (await response.json().catch(() => null)) as {
    data?: T
    error?: { code?: string; message?: string; details?: PolicyError["details"] }
  } | null
  if (response.ok && body?.data !== undefined) return body.data
  const unknownPath = response.status === 401 || (response.status === 404 && !body?.error?.code)
  const code = unknownPath || response.status === 404 ? notOnServer : (body?.error?.code ?? "unavailable")
  const message = sentences[code] ?? body?.error?.message ?? (response.status === 403 ? sentences.forbidden : undefined)
  throw new PolicyError(
    response.status,
    code,
    message ?? `${step} failed: Paperboat is temporarily unavailable. Try again.`,
    body?.error?.details ?? {},
  )
}

const ws = (workspace: string) => `workspaces/${encodeURIComponent(workspace)}`

export const policyQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["policy", workspace],
    queryFn: ({ signal }) => call<Policy>("Loading the policy", `${ws(workspace)}/policy`, { signal }),
    enabled: Boolean(workspace),
    retry: (count, error) => !(error instanceof ApiError && error.status >= 400 && error.status < 500) && count < 2,
  })

/** Machine tags in the console (Lane K's tag API, server 2026.10.23.44). */
export const tagsLive = true

export type WorkspaceTags = {
  tags: { name: string; machines: string[] }[]
  can_edit: "all" | "own_machines" | "none"
  policy_version: number
}

/** The workspace's tags (`tag:<name>`) and the machines carrying each; the policy file is their store. */
export const tagsQuery = (workspace: string) =>
  queryOptions({
    queryKey: queryKeys.tags(workspace),
    queryFn: ({ signal }) => call<WorkspaceTags>("Loading tags", `${ws(workspace)}/tags`, { signal }),
    enabled: tagsLive && Boolean(workspace),
  })

/**
 * Sets one machine's tags (new tags are created; a tag left without machines is dropped unless a
 * rule uses it). Like a policy save: 412 lists the access it changes, to confirm; 409 means reload.
 */
export function setMachineTags(
  workspace: string,
  machineId: string,
  tags: string[],
  expected: number,
  confirm?: string,
) {
  return call<{ tags: string[]; policy_version: number }>(
    "Saving tags",
    `${ws(workspace)}/machines/${encodeURIComponent(machineId)}/tags`,
    { method: "PUT", json: { tags, expected_version: expected, ...(confirm && { confirm }) } },
  )
}

/** `tag:<name>` names: lowercase letters, digits, . _ -; 1–63 characters, starting with a letter or digit. */
export const tagNamePattern = /^tag:[a-z0-9][a-z0-9._-]{0,62}$/

export const policyVersionQuery = (workspace: string, version: number) =>
  queryOptions({
    queryKey: ["policy", workspace, "version", version],
    queryFn: ({ signal }) =>
      call<Policy>("Loading the version", `${ws(workspace)}/policy?version=${version}`, { signal }),
    staleTime: Infinity,
  })

/** A policy's saved versions, newest first, one server page at a time (load more until the server's total). */
export const policyVersionsQuery = (workspace: string) =>
  infiniteQueryOptions({
    queryKey: ["policy", workspace, "versions"],
    queryFn: async ({ pageParam, signal }) => {
      const query = new URLSearchParams({ limit: "50", ...(pageParam && { cursor: pageParam }) })
      return call<{ versions: PolicyVersion[]; total: number; next_cursor?: string }>(
        "Loading the history",
        `${ws(workspace)}/policy/versions?${query}`,
        { signal },
      )
    },
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: Boolean(workspace),
  })

export function validatePolicy(workspace: string, policy: string, signal?: AbortSignal) {
  return call<Validation>("Checking the policy", `${ws(workspace)}/policy/validate`, {
    method: "POST",
    json: { policy },
    signal,
  })
}

export function savePolicy(
  workspace: string,
  fields: { policy: string; expected_version: number; message?: string; confirm?: string },
) {
  return call<Policy>("Saving the policy", `${ws(workspace)}/policy`, { method: "PUT", json: fields })
}

export function restorePolicy(
  workspace: string,
  fields: { version: number; expected_version: number; message?: string; confirm?: string },
) {
  return call<Policy>("Restoring the version", `${ws(workspace)}/policy/restore`, { method: "POST", json: fields })
}

export function explainPolicy(
  workspace: string,
  fields: { policy?: string; src?: string; dst?: string; action?: string; port?: number; api_token?: string },
) {
  return call<Explanation>("Testing access", `${ws(workspace)}/policy/explain`, { method: "POST", json: fields })
}

/**
 * A workspace's API tokens in one `state` (live, or revoked and expired), newest first, one server page at
 * a time (load more until the server's total). Every page also carries both states' totals.
 */
export const apiTokensQuery = (workspace: string, state: "active" | "ended", enabled = true) =>
  infiniteQueryOptions({
    queryKey: queryKeys.apiTokens(workspace, state),
    queryFn: async ({ pageParam, signal }) => {
      const query = new URLSearchParams({ limit: "50", state, ...(pageParam && { cursor: pageParam }) })
      return call<{
        api_tokens: APIToken[]
        total: number
        active_total: number
        ended_total: number
        next_cursor?: string
      }>(
        "Loading API tokens",
        `${ws(workspace)}/api-tokens?${query}`,
        { signal },
      )
    },
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: Boolean(workspace) && enabled,
    retry: (count, error) => !(error instanceof ApiError && error.status >= 400 && error.status < 500) && count < 2,
  })

export function createApiToken(
  workspace: string,
  fields: { name: string; expires_in_days: number; scope?: ScopeEntry[] },
) {
  return call<{ api_token: APIToken; secret: string }>("Creating the token", `${ws(workspace)}/api-tokens`, {
    method: "POST",
    json: fields,
  })
}

export function revokeApiToken(workspace: string, id: string) {
  return call<{ api_token: APIToken }>("Revoking the token", `${ws(workspace)}/api-tokens/${encodeURIComponent(id)}`, {
    method: "DELETE",
  })
}

/** Machine actions in the order the console lists them; `*` is all of them. */
export const machineActions = [
  "terminal",
  "exec",
  "managed_ssh",
  "files",
  "preview_manage",
  "tunnel_manage",
  "private_access",
  "inspect",
  "replay",
] as const

export const actionLabels: Record<string, string> = {
  "*": "Everything",
  terminal: "Terminal",
  exec: "Run commands",
  managed_ssh: "SSH, SCP and SFTP",
  files: "Files",
  preview_manage: "Previews",
  tunnel_manage: "Tunnels",
  private_access: "Private access",
  inspect: "Inspect requests",
  replay: "Replay requests",
  env_read: "Read team ENV",
  env_write: "Change team ENV",
  record: "Terminal sessions recorded",
  recording_view: "Replay recordings",
}

export const reasonLabels: Record<string, string> = {
  owner: "The machine's owner always has access",
  rule: "Allowed by a rule",
  denied_by_rule: "Denied by a rule",
  no_rule: "No rule allows it",
  port_not_allowed: "The rule does not cover this port",
  private_access_off: "Private access is off for this team",
  outside_token_scope: "Outside the API token's scope",
}

export const guardrailLabels: Record<string, string> = {
  team_suspended: "The team is suspended",
  membership_inactive: "The member is not active in this workspace",
  machine_not_in_workspace: "The machine is not in this workspace",
  team_not_entitled: "The team's plan does not include this",
  machine_service_off: "The service is switched off on the machine",
}

/** Tag and group names: lowercase letters, digits, `.`, `_` and `-`, up to 63. API token names too. */
export const namePattern = /^[a-z0-9][a-z0-9._-]{0,62}$/

// ---- The file, read in the browser (HuJSON: comments and trailing commas) ----

export type PolicyRule = {
  src: string[]
  dst: string[]
  allow?: string[]
  deny?: string[]
  ports?: (number | string)[]
  note?: string
}

export type PolicyFile = {
  allow_private_access?: boolean
  groups?: Record<string, string[]>
  tags?: Record<string, string[]>
  rules?: PolicyRule[]
  tests?: unknown[]
}

/** Removes comments and trailing commas, leaving strings alone, so `JSON.parse` reads HuJSON. */
export function standardize(text: string): string {
  let out = ""
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (c === '"') {
      const start = i
      for (i++; i < text.length && text[i] !== '"'; i++) if (text[i] === "\\") i++
      out += text.slice(start, i + 1)
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++
      out += "\n"
    } else if (c === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2)
      i = end < 0 ? text.length : end + 1
    } else if (c === "," && /^\s*[}\]]/.test(text.slice(i + 1))) {
      continue
    } else out += c
  }
  return out
}

export function hasComments(text: string): boolean {
  return standardize(text).replace(/\s+/g, "") !== stripCommasOnly(text).replace(/\s+/g, "")
}

function stripCommasOnly(text: string) {
  return text.replace(/,(\s*[}\]])/g, "$1")
}

/** The parsed file, or undefined while it does not parse (the editor shows the server's errors). */
export function parsePolicy(text: string): PolicyFile | undefined {
  try {
    const value = JSON.parse(standardize(text)) as unknown
    return value && typeof value === "object" && !Array.isArray(value) ? (value as PolicyFile) : undefined
  } catch {
    return undefined
  }
}

/** A line diff (longest common subsequence), for comparing policy versions. Sizes are bounded by the file limit. */
export function lineDiff(before: string, after: string): { kind: " " | "-" | "+"; text: string }[] {
  const a = before.split("\n")
  const b = after.split("\n")
  const table = Array.from({ length: a.length + 1 }, () => new Uint32Array(b.length + 1))
  for (let i = a.length - 1; i >= 0; i--)
    for (let j = b.length - 1; j >= 0; j--)
      table[i][j] = a[i] === b[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1])
  const out: { kind: " " | "-" | "+"; text: string }[] = []
  let i = 0
  let j = 0
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) {
      out.push({ kind: " ", text: a[i] })
      i++
      j++
    } else if (j < b.length && (i === a.length || table[i][j + 1] >= table[i + 1][j]))
      out.push({ kind: "+", text: b[j++] })
    else out.push({ kind: "-", text: a[i++] })
  }
  return out
}

/** " by <who>" for the policy's last save; empty when the server saved it. */
export function savedBy(policy: Policy) {
  if (!policy.updated_by) return ""
  const who = policy.updated_by.replace(/^user:/, "")
  return ` by ${who}${policy.updated_with_api_token ? ` with API token ${policy.updated_with_api_token}` : ""}`
}

/** Shown wherever the signed-in member may look but not change the policy. */
export function readOnlyNote(policy: Policy) {
  if (policy.can_edit === "all") return null
  return policy.can_edit === "own_machines"
    ? "You can change access only for your own machines, from the Team page."
    : "Only owners and admins can change access."
}
