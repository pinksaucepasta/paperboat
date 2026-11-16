"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, apiAction } from "@/lib/api"
import { cursorPage } from "@/lib/cursor-pages"
import { queryKeys } from "@/lib/query-keys"

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
  assignment_version?: number
  consent_state?: string
  remote_revision?: string
  adopted_team_id?: string
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

type StatusPage = { environments: ConfigEnvironment[] | null; pagination: { total: number; next_cursor?: string } }

const readStatus = (raw: StatusPage) => ({
  items: raw.environments,
  total: raw.pagination.total,
  next_cursor: raw.pagination.next_cursor,
})

/** One page of machines' sync, searched and filtered by the server (`machine` is one machine's row). */
export const configStatusQuery = (options: { q?: string; state?: string; machine?: string; page?: number }) =>
  queryOptions({
    queryKey: queryKeys.configStatus(options),
    queryFn: ({ signal }) =>
      cursorPage<ConfigEnvironment, StatusPage>(
        "config-sync/status",
        {
          ...(options.q && { q: options.q }),
          ...(options.state && { state: options.state }),
          ...(options.machine && { machine_id: options.machine }),
        },
        options.page ?? 1,
        signal,
        readStatus,
      ),
    // Keep the last page while paging or searching, never to another machine or list (state).
    placeholderData: (previous, previousQuery) => {
      const before = previousQuery?.queryKey.at(-1) as typeof options | undefined
      return before?.machine === options.machine && before?.state === options.state ? previous : undefined
    },
    refetchInterval: 15_000,
  })

export const directionLabels: Record<string, string> = {
  bidirectional: "Two-way",
  pull_only: "From the repository",
  push_only: "To the repository",
  disabled: "Off",
}

/** Why a path is listed (skipped, waiting for review, in conflict), in plain words; unknown codes say nothing. */
const reasons: Record<string, string> = {
  added: "New file",
  changed: "Changed",
  changed_both: "Changed on both sides",
  delete_modify: "Deleted on one side, changed on the other",
  concurrent_update: "Changed while syncing",
  case_collision: "Name differs only in case",
  excluded: "Excluded",
  file_changing: "Still being written",
  max_batch_bytes: "Too much to sync at once",
  max_file_bytes: "Too large",
  special_file: "Not a regular file",
  unsafe_permissions: "Readable by others",
  unsafe_symlink: "Link points outside the folder",
}
export const reasonLabel = (code: string): string | undefined => reasons[code]

/** What to do to recover, in plain words; unknown codes are left out. */
const actions: Record<string, string> = {
  apply_configuration: "apply the configuration again",
  check_repository_access: "check the repository's access",
  check_status: "check the status on the machine",
  configure_repository_credentials: "connect the repository again",
  fix_configuration: "fix the configuration",
  fix_manifest: "fix the config manifest",
  fix_path_rules: "fix the path rules",
  keep_local: "keep the machine's version",
  keep_remote: "keep the repository's version",
  observe_remote: "wait for the repository to settle",
  retry: "try again",
  review_revision: "review the change",
  wait_for_lease: "wait for the other machine to finish",
  wait_for_rollout: "wait for the update to finish",
}
export const actionLabels = (codes: string[]): string[] => codes.flatMap((code) => actions[code] ?? [])

/** A sync error in plain words; one this console doesn't know yet is a plain "Couldn’t sync". */
const errors: Record<string, string> = {
  access_decrypt_failed: "Couldn’t read the repository credentials",
  config_conflict: "Conflict",
  config_sync_failed: "Couldn’t sync",
  config_writes_disabled: "Writing config is turned off",
  credential_expired: "Repository access expired",
  network_error: "Couldn’t reach the repository",
  provider_outcome_uncertain: "Couldn’t confirm the last sync",
}
const errorLabel = (code: string) => errors[code] ?? "Couldn’t sync"

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
        label: environment.error_code ? errorLabel(environment.error_code) : "Couldn’t sync",
        attention: true,
        danger: true,
      }
  }
}

const json = { "content-type": "application/json" }
const post = <T>(path: string, body: unknown) =>
  api<T>(path, { method: "POST", headers: json, body: JSON.stringify(body) })
const machinePath = (machineId: string) => `machines/${encodeURIComponent(machineId)}/config-assignment`

/** What the person agrees to before a machine syncs; the sentences are the server's. */
export type ConfigWarning = {
  revision: string
  repository_visibility: string
  history_retention: string
  access_consequence: string
  offline_behavior: string
  conflict_behavior: string
  disable_action: string
}
export const configWarningQuery = (machineId: string) =>
  queryOptions({
    queryKey: ["config-sync", "warning", machineId] as const,
    queryFn: ({ signal }) => api<ConfigWarning>(`${machinePath(machineId)}/warning?workspace=personal`, { signal }),
  })

/** The fences (`assignment_version`, `warning_revision`, `remote_revision`) come from the machine's status row. */
export const acceptConfigConsent = (environment: ConfigEnvironment, warningRevision: string) =>
  post(`${machinePath(environment.machine_id)}/consent?workspace=personal`, {
    warning_revision: warningRevision,
    expected_version: environment.assignment_version,
  })
export const approveConfigRevision = (environment: ConfigEnvironment) =>
  post(`${machinePath(environment.machine_id)}/approve?workspace=personal`, {
    remote_revision: environment.remote_revision,
    expected_version: environment.assignment_version,
  })
export const removeConfigConsent = (environment: ConfigEnvironment) =>
  apiAction(
    `${machinePath(environment.machine_id)}/consent?${new URLSearchParams({
      workspace: "personal",
      expected_version: String(environment.assignment_version),
    })}`,
    { method: "DELETE" },
  )

export type ForceAction = "force_pull" | "force_push"
export const forceWords: Record<ForceAction, string> = { force_pull: "FORCE PULL", force_push: "FORCE PUSH" }
/** Replaces the machine's files with the repository's (pull) or the repository's with the machine's (push). */
export const forceConfig = (environment: ConfigEnvironment, action: ForceAction) =>
  post(`config-sync/environments/${encodeURIComponent(environment.environment_id)}/force?workspace=personal`, {
    scope: "config",
    action,
    confirmation: forceWords[action],
    expected_remote_revision: environment.remote_revision,
    expected_assignment_version: environment.assignment_version,
  })

export type ConfigRepository = { id: string; provider: string; external_ref: string; display_name: string; state: string }
export const configRepositoriesPageSize = 20
export const configRepositoriesQuery = (page: number) =>
  queryOptions({
    queryKey: ["config-sync", "repositories", page] as const,
    queryFn: ({ signal }) =>
      api<{ items: ConfigRepository[] | null; pagination: { total: number } }>(
        `config-repositories?${new URLSearchParams({
          workspace: "personal",
          limit: String(configRepositoriesPageSize),
          offset: String((page - 1) * configRepositoriesPageSize),
        })}`,
        { signal },
      ),
    placeholderData: (previous) => previous,
  })
export type RepositoryCandidate = { provider: string; external_id: string; display_name: string; default_branch: string }
/** One page of the GitHub repositories you could connect, by cursor (the list has no total). */
export const repositoryCandidatesQuery = (cursor: string) =>
  queryOptions({
    queryKey: ["config-sync", "candidates", cursor] as const,
    queryFn: ({ signal }) =>
      api<{ items: RepositoryCandidate[] | null; next_cursor?: string }>(
        `config-repositories/candidates?${new URLSearchParams({ workspace: "personal", limit: "20", ...(cursor && { cursor }) })}`,
        { signal },
      ),
  })
export const connectRepository = (body: {
  provider: "github" | "git"
  external_ref: string
  display_name?: string
  branch: string
}) => post<ConfigRepository>("config-repositories?workspace=personal", body)
export const disconnectRepository = (id: string) =>
  apiAction(`config-repositories/${encodeURIComponent(id)}?workspace=personal`, { method: "DELETE" })

/** A team's default repository (admins set it) and this account's adoption of it. */
export type TeamConfigDefault = {
  team_id: string
  display_name: string
  external_repository_id: string
  branch: string
  version: number
}
export type ConfigAdoption = TeamConfigDefault & { repository_id: string; adopted_version: number; update_pending: boolean }
export const teamConfigDefaultQuery = (teamId: string) =>
  queryOptions({
    queryKey: ["config-sync", "team-default", teamId] as const,
    queryFn: ({ signal }) =>
      api<TeamConfigDefault>(`teams/${encodeURIComponent(teamId)}/config-default?workspace=${encodeURIComponent(teamId)}`, {
        signal,
      }),
    retry: false,
  })
export const setTeamConfigDefault = (teamId: string, repositoryId: string, expectedVersion: number) =>
  api<TeamConfigDefault>(`teams/${encodeURIComponent(teamId)}/config-default?workspace=${encodeURIComponent(teamId)}`, {
    method: "PUT",
    headers: json,
    body: JSON.stringify({ repository_id: repositoryId, expected_version: expectedVersion }),
  })
export const clearTeamConfigDefault = (teamId: string, expectedVersion: number) =>
  apiAction(
    `teams/${encodeURIComponent(teamId)}/config-default?${new URLSearchParams({ workspace: teamId, expected_version: String(expectedVersion) })}`,
    { method: "DELETE" },
  )
export const configAdoptionQuery = queryOptions({
  queryKey: ["config-sync", "adoption"] as const,
  queryFn: ({ signal }) => api<ConfigAdoption>("config-sync/team-default-adoption?workspace=personal", { signal }),
  retry: false,
})
export const adoptTeamDefault = (teamId: string, version: number) =>
  api<ConfigAdoption>(`config-sync/team-default-adoption?workspace=${encodeURIComponent(teamId)}`, {
    method: "PUT",
    headers: json,
    body: JSON.stringify({ team_id: teamId, default_version: version }),
  })
export const unadoptTeamDefault = () => apiAction("config-sync/team-default-adoption?workspace=personal", { method: "DELETE" })
