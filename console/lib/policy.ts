"use client"

import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query"
import { traceFrom, type ServerTrace } from "@/lib/error-reference"
import { ApiError, checked, confirmSession, idempotencyKey, send, type Data, type Schemas } from "@/lib/api"
import { queryKeys } from "@/lib/query-keys"

// Workspace policy and API tokens: types from the OpenAPI document; contract: shared/policy/README.md "HTTP API".

export type PolicyNames = Schemas["PolicyNames"]
export type Policy = Schemas["PolicyView"]
export type CanEdit = Policy["can_edit"]
export type PolicyProblem = Schemas["PolicyProblem"]
/** `ports` is private_access only, e.g. "22,80,443,1024-65535". */
export type AccessChange = Schemas["PolicyChange"]
export type Validation = Schemas["PolicyValidation"]

export type ScopeEntry = Schemas["ApiTokenScopeEntry"]
export type APIToken = Schemas["ApiToken"]

/** A policy error with the server's `details` (the error list, or the access changes to confirm). */
export class PolicyError extends ApiError {
  constructor(
    status: number,
    code: string,
    message: string,
    readonly details: { errors?: PolicyProblem[]; access_changes?: AccessChange[]; confirm?: string },
    trace?: ServerTrace,
  ) {
    super(status, code, message, undefined, undefined, trace)
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
    error?: {
      code?: string
      message?: string
      details?: PolicyError["details"]
      support_reference?: string
      request_id?: string
    }
  } | null
  if (response.ok && body?.data !== undefined) return checked(rest.method, path, body.data)
  const unknownPath = response.status === 401 || (response.status === 404 && !body?.error?.code)
  const code = unknownPath || response.status === 404 ? notOnServer : (body?.error?.code ?? "unavailable")
  const message = sentences[code] ?? body?.error?.message ?? (response.status === 403 ? sentences.forbidden : undefined)
  throw new PolicyError(
    response.status,
    code,
    message ?? `${step} failed: Paperboat is temporarily unavailable. Try again.`,
    body?.error?.details ?? {},
    traceFrom(response, body?.error),
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

/** Machine tags in the console (the tag API, server 2026.10.23.44). */
export const tagsLive = true

export type WorkspaceTags = Schemas["WorkspaceTags"]

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
  return call<Schemas["MachineTagsView"]>(
    "Saving tags",
    `${ws(workspace)}/machines/${encodeURIComponent(machineId)}/tags`,
    { method: "PUT", json: { tags, expected_version: expected, ...(confirm && { confirm }) } },
  )
}

/** `tag:<name>` names: lowercase letters, digits, . _ -; 1–63 characters, starting with a letter or digit. */
export const tagNamePattern = /^tag:[a-z0-9][a-z0-9._-]{0,62}$/

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

/**
 * A workspace's API tokens in one `state` (live, or revoked and expired), newest first, one server page at
 * a time (load more until the server's total). Every page also carries both states' totals.
 */
export const apiTokensQuery = (workspace: string, state: "active" | "ended", enabled = true) =>
  infiniteQueryOptions({
    queryKey: queryKeys.apiTokens(workspace, state),
    queryFn: async ({ pageParam, signal }) => {
      const query = new URLSearchParams({ limit: "50", state, ...(pageParam && { cursor: pageParam }) })
      return call<Data<"/v1/workspaces/{ws}/api-tokens">>(
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
  return call<Data<"/v1/workspaces/{ws}/api-tokens", "post">>("Creating the token", `${ws(workspace)}/api-tokens`, {
    method: "POST",
    json: fields,
  })
}

export function revokeApiToken(workspace: string, id: string) {
  return call<Data<"/v1/workspaces/{ws}/api-tokens/{id}", "delete">>("Revoking the token", `${ws(workspace)}/api-tokens/${encodeURIComponent(id)}`, {
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
