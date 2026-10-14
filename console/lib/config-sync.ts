"use client"

import { queryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"

/*
 * Config sync keeps chosen files on each machine (dotfiles and the like) in a Git repository. The
 * machines report their state; the console only shows it. Conflicts are resolved on the machine
 * (`pb config conflict`), never here.
 */

/** One reported path: a conflict, a change waiting for approval, or a skipped file. */
export type ConfigPath = { path: string; bytes?: number; reason: string; revision?: string }

/** controlplane.ConfigAccountEnvironment: one machine's sync. */
export type ConfigEnvironment = {
  machine_id: string
  environment_id: string
  alias: string
  state: string
  mode?: "bidirectional" | "pull_only" | "push_only" | "disabled" | string
  repository_name?: string
  repository_id?: string
  automatic_updates: boolean
  managed_path_count: number
  conflicts: ConfigPath[] | null
  review: ConfigPath[] | null
  skipped: ConfigPath[] | null
  error_code?: string
  recovery_actions: string[] | null
  last_successful_sync_at?: string
  last_attempt_at?: string
  updated_at?: string
}

export type ConfigAccountStatus = { state: string; environments: ConfigEnvironment[] }

export const configStatusQuery = queryOptions({
  queryKey: ["config-sync"],
  queryFn: () => api<ConfigAccountStatus>("config-sync/status?limit=200"),
  refetchInterval: 15_000,
})

export const directionLabels: Record<string, string> = {
  bidirectional: "Two-way",
  pull_only: "From the repository",
  push_only: "To the repository",
  disabled: "Off",
}

/** A reason or error code, readable: `binary_file` → "Binary file". */
export const humanize = (code: string) => {
  const text = code.replace(/_/g, " ")
  return text.charAt(0).toUpperCase() + text.slice(1)
}

/**
 * A machine's sync in a few words, whether it's fine, and whether it's a problem (conflicts,
 * errors) rather than something waiting for you. Conflicts and pending reviews come first.
 */
export function syncState(environment: ConfigEnvironment): {
  ok: boolean
  label: string
  attention: boolean
  danger: boolean
} {
  const conflicts = environment.conflicts?.length ?? 0
  const review = environment.review?.length ?? 0
  if (conflicts)
    return {
      ok: false,
      label: `${conflicts} ${conflicts === 1 ? "conflict" : "conflicts"}`,
      attention: true,
      danger: true,
    }
  if (review) return { ok: false, label: "Waiting for approval", attention: true, danger: false }
  switch (environment.state) {
    case "healthy":
    case "watching":
      return { ok: true, label: "In sync", attention: false, danger: false }
    case "syncing":
    case "restoring":
    case "pending":
      return { ok: false, label: "Syncing", attention: false, danger: false }
    case "offline":
      return { ok: false, label: "Offline", attention: false, danger: false }
    case "disabled":
      return { ok: false, label: "Off", attention: false, danger: false }
    case "consent_required":
      return { ok: false, label: "Needs consent on the machine", attention: true, danger: false }
    case "revoked":
      return { ok: false, label: "Access revoked", attention: true, danger: true }
    case "warning":
      return { ok: false, label: "Warning", attention: true, danger: false }
    case "sync_uncertain":
      return { ok: false, label: "Couldn’t confirm the last sync", attention: true, danger: true }
    default:
      return {
        ok: false,
        label: environment.error_code ? humanize(environment.error_code) : "Error",
        attention: true,
        danger: true,
      }
  }
}
