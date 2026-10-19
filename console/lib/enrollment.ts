"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, type Machine } from "@/lib/api"

export type Shell = "posix" | "powershell"

export type EnrollmentState =
  | "awaiting_bootstrap"
  | "awaiting_approval"
  | "approved"
  | "material_issued"
  | "installing"
  | "connecting"
  | "ready"
  | "cancelled"
  | "expired"
  | "denied"
  | "failed_retryable"
  | "revoked"
  | "disconnected"
  | "deleted"
  | "auth_consumed"

export type Enrollment = { id: string; state: EnrollmentState; machine_id?: string; expires_at: string }

type EnrollmentStart = Enrollment & { bootstrap_token: string }

/** States after which the enrollment will not progress on its own. */
export const finishedStates = new Set<EnrollmentState>([
  "ready",
  "cancelled",
  "expired",
  "denied",
  "failed_retryable",
  "revoked",
  "deleted",
])

// Mirrors `machinealias.Valid` in paperboat-server/internal/machinealias/alias.go.
const reservedNames = new Set(
  "api auth bugreport config connect doctor e2ee exec inbox internal login logout machine pb ping preview serve session sessions ssh status transfer wait www".split(
    " ",
  ),
)

/** Lowercases and turns anything outside a-z0-9 into single dashes, as the server does. */
export function normalizeName(value: string): string {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+/, "")
    .slice(0, 63)
}

export function nameProblem(name: string): string | undefined {
  const value = name.replace(/-+$/, "")
  if (!value) return
  if (reservedNames.has(value)) return `“${value}” is reserved`
}

/** The server publishes `curl -fsSL <base>/install | bash …`; the install URL is the part to reuse. */
export function installUrlFrom(bootstrapCommand: string | undefined): string | undefined {
  return bootstrapCommand?.match(/https:\/\/[^\s'"]+\/install\b/)?.[0]
}

export function installCommand(installUrl: string, shell: Shell, token?: string, name?: string): string {
  const parameter = token ? `?p=${name ? `${name}-${token}` : token}` : ""
  const url = installUrl + parameter
  return shell === "posix" ? `curl -fsSL '${url}' | bash` : `powershell -c "iex (irm '${url}')"`
}

/** `workspace` is "personal" (the default) or the team the machine joins when it pairs. */
export async function startEnrollment({
  shell,
  workspace,
}: {
  shell: Shell
  workspace: string
}): Promise<EnrollmentStart> {
  return api<EnrollmentStart>("machine-enrollments", {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify(workspace === "personal" ? { shell } : { shell, workspace }),
  })
}

export function cancelEnrollment(id: string): Promise<unknown> {
  return api(`machine-enrollments/${encodeURIComponent(id)}/cancel`, { method: "POST" })
}

export const enrollmentQuery = (id: string) =>
  queryOptions({
    queryKey: ["enrollments", id],
    queryFn: () => api<Enrollment>(`machine-enrollments/${encodeURIComponent(id)}`),
    refetchInterval: (query) => (query.state.data && finishedStates.has(query.state.data.state) ? false : 2000),
  })

/** The installers cannot carry a description, so it is applied once the machine exists. */
export async function describeMachine(id: string, description: string): Promise<void> {
  const machine = await api<Machine>(`machines/${encodeURIComponent(id)}`)
  await api(`machines/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ alias: machine.alias, description }),
  })
}
