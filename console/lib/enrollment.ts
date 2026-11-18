"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, type Schemas } from "@/lib/api"

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

/** `failure_code`: why pairing was refused, for failed_retryable (e.g. machine_limit_reached). */
export type Enrollment = Omit<Schemas["MachineEnrollment"], "state"> & { state: EnrollmentState }

type EnrollmentStart = Enrollment & Pick<Schemas["MachineEnrollmentStart"], "bootstrap_token" | "bootstrap_command" | "server_url">

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

/** The server publishes `curl -fsSL <installer> | sh`; the installer address is the part to reuse. */
export function installUrlFrom(bootstrapCommand: string | undefined): string | undefined {
  return bootstrapCommand?.match(/https:\/\/[^\s'"|]+/)?.[0]
}

/**
 * One address for both shells (it serves install.sh to curl and install.ps1 to PowerShell). The same token goes to
 * either one in the installer's environment, never in a URL: `PB_TOKEN` and, for a chosen name, `PB_NAME`.
 */
export function installCommand(installUrl: string, shell: Shell, token?: string, name?: string): string {
  const variables = token ? [["PB_TOKEN", token], ...(name ? [["PB_NAME", name]] : [])] : []
  return shell === "posix"
    ? `curl -fsSL ${installUrl} | ${variables.map(([key, value]) => `${key}='${value}' `).join("")}sh`
    : `${variables.map(([key, value]) => `$env:${key}='${value}'; `).join("")}irm ${installUrl} | iex`
}

/** `workspace` is "personal" (the default) or the team the machine joins when it pairs. */
export async function startEnrollment({ workspace }: { workspace: string }): Promise<EnrollmentStart> {
  return api<EnrollmentStart>("machine-enrollments", {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify(workspace === "personal" ? {} : { workspace }),
  })
}

export function cancelEnrollment(id: string): Promise<unknown> {
  return api(`machine-enrollments/${encodeURIComponent(id)}/cancel`, { method: "POST" })
}

export const enrollmentQuery = (id: string) =>
  queryOptions({
    queryKey: ["enrollments", id],
    queryFn: ({ signal }) => api<Enrollment>(`machine-enrollments/${encodeURIComponent(id)}`, { signal }),
    refetchInterval: (query) => (query.state.data && finishedStates.has(query.state.data.state) ? false : 2000),
  })
