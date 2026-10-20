"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, apiAction, ApiError, idempotencyKey, send, type Data } from "@/lib/api"
import type { TerminalSession } from "@/lib/terminal/use-terminal-connection"

export type AccessPolicy = Data<"/v1/terminal-sessions/{session_id}/browser-access-policy">
export type CodeAccess = Data<"/v1/terminal-sessions/{session_id}/code-access">
export type Participant = TerminalSession["participants"][number]

const sessionPath = (id: string) => `terminal-sessions/${encodeURIComponent(id)}`
const json = (method: string, body: unknown): RequestInit => ({
  method,
  headers: { "content-type": "application/json" },
  body: JSON.stringify(body),
})

export const terminalKeys = {
  list: ["terminal-sessions"] as const,
  session: (id: string) => ["terminal-sessions", id] as const,
  policy: (id: string) => ["terminal-sessions", id, "policy"] as const,
  code: (id: string) => ["terminal-sessions", id, "code-access"] as const,
}

/** Every terminal you own or that is shared with you. */
export const terminalsQuery = queryOptions({
  queryKey: terminalKeys.list,
  queryFn: async () => (await api<Data<"/v1/terminal-sessions">>("terminal-sessions?limit=200")).sessions,
  refetchInterval: 15_000,
})

/**
 * One terminal with its live participants. There is no participant event stream, so the page
 * polls; the terminal itself never restarts for it (see useTerminalConnection).
 */
export const terminalQuery = (id: string) =>
  queryOptions({
    queryKey: terminalKeys.session(id),
    queryFn: () => api<TerminalSession>(`${sessionPath(id)}/sharing`),
    refetchInterval: 4_000,
  })

/** Owner only: who may open this terminal in a browser. */
export const policyQuery = (id: string) =>
  queryOptions({
    queryKey: terminalKeys.policy(id),
    queryFn: () => api<AccessPolicy>(`${sessionPath(id)}/browser-access-policy`),
  })

/** Owner only: the join code for people without an account. */
export const codeAccessQuery = (id: string) =>
  queryOptions({
    queryKey: terminalKeys.code(id),
    queryFn: () => api<CodeAccess>(`${sessionPath(id)}/code-access`),
  })

export function savePolicy(
  id: string,
  policy: AccessPolicy,
  next: { audience: AccessPolicy["audience"]; teamId?: string; emails: string[]; interactive: string[] },
) {
  return api<AccessPolicy>(
    `${sessionPath(id)}/browser-access-policy`,
    json("PUT", {
      expected_generation: policy.generation,
      audience: next.audience,
      team_id: next.audience === "team" ? next.teamId : "",
      allowed_emails: next.audience === "specific_emails" ? next.emails : [],
      interactive_emails: next.audience === "specific_emails" ? next.interactive : [],
      // The server requires an explicit yes before anyone outside the owner can run commands.
      confirm_interactive_access: next.audience === "specific_emails" && next.interactive.length > 0,
    }),
  )
}

export function changeCodeAccess(
  id: string,
  current: CodeAccess,
  change: {
    enabled: boolean
    reset?: boolean
    role: CodeAccess["role"]
    expiresIn: 3600 | 28800 | 86400
    confirmControl?: boolean
  },
) {
  return api<CodeAccess>(
    `${sessionPath(id)}/code-access`,
    json("PUT", {
      expected_generation: current.generation,
      enabled: change.enabled,
      reset: change.reset ?? false,
      role: change.role,
      expires_in_seconds: change.expiresIn,
      confirm_interactive_access: change.role === "interactive" && !!change.confirmControl,
    }),
  )
}

/** Owner only: disconnect someone and keep them out, whatever audience they came in through. */
export function removeParticipant(session: TerminalSession, accountId: string) {
  return api<TerminalSession>(
    `${sessionPath(session.id)}/participants/${encodeURIComponent(accountId)}/remove`,
    json("POST", { operation_id: idempotencyKey(), expected_generation: session.sharing.generation }),
  )
}

/** Ends the shell on the machine for everyone. The server may queue it (202) until the machine confirms. */
export function closeTerminal(machineId: string, sessionId: string) {
  return apiAction(
    `machines/${encodeURIComponent(machineId)}/terminal-sessions/${encodeURIComponent(sessionId)}/close`,
    { method: "POST" },
  )
}

/**
 * For people joining with a code: like `api`, but a 401 is an answer ("no access yet"), not a
 * reason to send them to sign-in, since they have no account.
 */
export async function guestApi<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await send(`/api/v1/${path}`, {
    ...init,
    cache: "no-store",
    headers: { accept: "application/json", ...init?.headers },
  })
  const body = (await response.json().catch(() => null)) as {
    data?: T
    error?: { code?: string; message?: string }
  } | null
  if (!response.ok || !body || body.data === undefined) {
    throw new ApiError(
      response.status,
      body?.error?.code ?? "unavailable",
      body?.error?.message ?? "Paperboat is temporarily unavailable.",
    )
  }
  return body.data
}

export type BrowserSession = { session: TerminalSession; access_method: "account" | "code" }

/** Who you are to this terminal: an account with access, or a guest admitted by code. */
export const browserSessionQuery = (id: string) =>
  queryOptions({
    queryKey: ["terminal-sessions", id, "browser-session"] as const,
    queryFn: () => guestApi<BrowserSession>(`${sessionPath(id)}/browser-session`),
    retry: false,
  })
