"use client"

import { queryOptions } from "@tanstack/react-query"
import { useSyncExternalStore } from "react"
import { api, ApiError } from "@/lib/api"

/*
 * Environment variables, end-to-end encrypted. The server keeps names, versions and delivery
 * status only; values are encrypted on the client with keys from your password-protected vault and
 * published as signed manifests, which each machine decrypts for itself. Personal globals reach
 * every machine; a machine override replaces a global of the same name on that machine.
 *
 * Listing works against the server today (`/v1/environment-variables`, per machine
 * `/v1/machines/{id}/environment-variables`). Unlocking the vault, revealing and saving values
 * need the CLI's vault code running in the browser (NOTES.md); until then those steps say so.
 */

export type Variable = {
  scope: "global" | "machine"
  machine_id?: string
  name: string
  version: number
  updated_at: string
}

/** environment.ScopeView: one scope's variables and, for a machine, whether it has the latest. */
export type ScopeView = {
  scope: "global" | "machine"
  machine_id?: string
  version: number
  variables: Variable[]
  /** Machine scopes: applied, pending (not picked up yet), offline, or an error state. */
  status?: string
  applied_state?: string
  error_code?: string
  observed_at?: string
}

/** Names as the server accepts them (environment.portableName). */
export const namePattern = /^[A-Za-z_][A-Za-z0-9_]*$/

export const envKeys = {
  global: (workspace: string) => ["environment", workspace, "global"] as const,
  machine: (workspace: string, machineId: string) => ["environment", workspace, "machine", machineId] as const,
}

export const globalScopeQuery = (workspace: string) =>
  queryOptions({
    queryKey: envKeys.global(workspace),
    queryFn: () => api<ScopeView>(`environment-variables?workspace=${encodeURIComponent(workspace)}`),
  })

export const machineScopeQuery = (workspace: string, machineId: string) =>
  queryOptions({
    queryKey: envKeys.machine(workspace, machineId),
    queryFn: () =>
      api<ScopeView>(
        `machines/${encodeURIComponent(machineId)}/environment-variables?workspace=${encodeURIComponent(workspace)}`,
      ),
    // Delivery status moves as the machine picks changes up.
    refetchInterval: 15_000,
  })

/** Parses a .env file: `NAME=value`, optional `export`, quotes, and `#` comments. */
export function parseDotenv(text: string): { name: string; value: string }[] {
  const entries: { name: string; value: string }[] = []
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim()
    if (!line || line.startsWith("#")) continue
    const match = /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/.exec(line)
    if (!match) continue
    let value = match[2]
    const quoted = /^(['"])([\s\S]*)\1$/.exec(value)
    if (quoted) value = quoted[1] === '"' ? quoted[2].replace(/\\n/g, "\n").replace(/\\"/g, '"') : quoted[2]
    else value = value.replace(/\s+#.*$/, "")
    entries.push({ name: match[1], value })
  }
  return entries
}

/*
 * The vault, held in this tab's memory only while unlocked (never stored), shared by the page's
 * components. Opening it needs the vault code in the browser; see the note at the top.
 */

type VaultState = { unlocked: false } | { unlocked: true; values: Map<string, string> }
let vault: VaultState = { unlocked: false }
const listeners = new Set<() => void>()
const emit = () => listeners.forEach((listener) => listener())

export function useVault() {
  return useSyncExternalStore(
    (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    () => vault,
    () => vault,
  )
}

export class VaultUnavailableError extends ApiError {
  constructor() {
    super(501, "vault_unavailable", "Unlocking in the browser isn’t available yet. Use pb env on a machine for now.")
  }
}

/**
 * Unlocks with the vault password. Reads the encrypted vault (`GET /v1/environment-vault`), then
 * opens it in the browser; the password never leaves this tab.
 */
export async function unlockVault(password: string) {
  if (!password) throw new ApiError(400, "invalid", "Enter your vault password.")
  await api("environment-vault")
  throw new VaultUnavailableError()
}

export function lockVault() {
  vault = { unlocked: false }
  emit()
}

/** The key a value is held under: the scope, then the name. */
export const valueKey = (scope: string, machineId: string | undefined, name: string) =>
  `${scope}:${machineId ?? ""}:${name}`

/**
 * Sets (or with `null`, removes) variables in one scope: encrypts the scope's new contents with
 * the vault's key and publishes them as the next signed manifest
 * (`PUT /v1/environment-manifests/global` or `…/machines/{id}`). Needs an unlocked vault.
 */
export const saveVariables: (
  target: { scope: "global" } | { scope: "machine"; machineId: string },
  changes: { name: string; value: string | null }[],
) => Promise<void> = async () => {
  // Only reachable once the vault opens in the browser; until then unlocking already says why.
  throw new VaultUnavailableError()
}
