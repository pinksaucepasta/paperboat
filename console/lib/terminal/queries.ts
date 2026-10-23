"use client"

import { keepPreviousData, queryOptions } from "@tanstack/react-query"
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

/** A page of the terminals you own or that are shared with you, newest activity first. */
export const terminalPageSize = 50

type TerminalPage = { sessions: TerminalSession[]; pagination: { total: number } }

function terminalsPath(params: Record<string, string>) {
  return `terminal-sessions?${new URLSearchParams(params)}`
}

/**
 * One page from the server, which searches (`q`), splits open from closed and counts (`total`).
 * `machine` narrows it to one machine's terminals.
 */
export const terminalsPageQuery = (options: { closed: boolean; q: string; page: number; machine?: string }) =>
  queryOptions({
    queryKey: [...terminalKeys.list, "page", options],
    queryFn: async () => {
      const page = await api<TerminalPage>(
        terminalsPath({
          limit: String(terminalPageSize),
          offset: String((options.page - 1) * terminalPageSize),
          closed: String(options.closed),
          ...(options.q && { q: options.q }),
          ...(options.machine && { machine_id: options.machine }),
        }),
      )
      return { items: page.sessions, total: page.pagination.total }
    },
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
  })

/** How many open and closed terminals there are, from the server's totals. */
export const terminalCountsQuery = (machine?: string) =>
  queryOptions({
    queryKey: [...terminalKeys.list, "counts", machine ?? ""],
    queryFn: async () => {
      const total = async (closed: boolean) =>
        (
          await api<TerminalPage>(
            terminalsPath({ limit: "1", closed: String(closed), live: "false", ...(machine && { machine_id: machine }) }),
          )
        ).pagination.total
      const [open, closed] = await Promise.all([total(false), total(true)])
      return { open, closed }
    },
    refetchInterval: 15_000,
  })

/**
 * One terminal with its live participants. Newer hosts push who is attached on the terminal's own
 * stream, and then this is only a safety net (`pushed`); older hosts don't, so the page polls.
 */
export const terminalQuery = (id: string, pushed = false) =>
  queryOptions({
    queryKey: terminalKeys.session(id),
    queryFn: () => api<TerminalSession>(`${sessionPath(id)}/sharing`),
    refetchInterval: pushed ? 60_000 : 4_000,
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
