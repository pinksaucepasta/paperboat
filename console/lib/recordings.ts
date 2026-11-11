"use client"

import { keepPreviousData, queryOptions } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"
import { vaultCrypto } from "@/lib/envvault/crypto"
import { currentVault } from "@/lib/envvault/vault"
import { hasComments, parsePolicy, type PolicyRule } from "@/lib/policy"
import type { components } from "@/lib/api-schema"

// Teams session recordings (contract: shared/recording). Recordings stay on their machine, encrypted to the team
// recording key; the server lists them and hands each permitted viewer their sealed copy of that key.

type Schemas = components["schemas"]
export type Recording = Schemas["Recording"]
export type RecordingKeyState = Schemas["RecordingKeyState"]
export type RecordingViewer = Schemas["RecordingViewer"]
export type Replay = Schemas["RecordingReplay"]
type RecordingKey = Schemas["RecordingKey"]
type RecordingGrant = Schemas["RecordingGrant"]
type ManualStart = Schemas["RecordingSettings"]["manual_start"]

export const recordingPageSize = 50

const team = (id: string) => `teams/${encodeURIComponent(id)}`
const json = { "content-type": "application/json" }

/** Server refusals worded for what the person can do next. */
export const recordingSentences: Record<string, string> = {
  team_plan_required: "Session recordings are part of the Teams plan.",
  recording_forbidden: "You can't replay recordings on this machine. Ask a team admin for replay access.",
  recording_key_unavailable: "The team has no recording key yet. A team admin sets it up under Team, Recordings.",
  recording_key_pending:
    "You can replay here, but nobody has given you a copy of the recording key yet. Ask a team member who can replay to open Recordings.",
  recording_key_conflict: "The recording key or a viewer's vault changed. Reload and try again.",
  recording_not_found: "This recording doesn't exist or isn't shared with you.",
  recording_deleted: "This recording was deleted from its machine.",
  machine_unavailable: "The recording's machine is offline. Replay works again once it's back online.",
  recording_unavailable: "Recording isn't available for this terminal. A team admin turns it on under Team, Recordings.",
  recording_active: "This terminal is already being recorded.",
  recording_inactive: "This terminal isn't being recorded.",
  credential_expired: "The replay link expired. Open the recording again.",
}

/** The error's sentence from the table above, else its own message. */
export const recordingMessage = (error: unknown) =>
  (error instanceof ApiError && recordingSentences[error.code]) ||
  (error instanceof Error ? error.message : "Session recordings are unavailable. Try again.")

export const recordingsQuery = (teamId: string, machine: string | undefined, page: number) =>
  queryOptions({
    queryKey: ["recordings", teamId, machine ?? "", page],
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({
        limit: String(recordingPageSize),
        offset: String((page - 1) * recordingPageSize),
        ...(machine && { machine }),
      })
      return api<Schemas["RecordingList"]>(`${team(teamId)}/recordings?${params}`, { signal })
    },
    enabled: Boolean(teamId),
    placeholderData: keepPreviousData,
    retry: (count, error) => !(error instanceof ApiError && error.status >= 400 && error.status < 500) && count < 2,
  })

export const recordingKeyQuery = (teamId: string) =>
  queryOptions({
    queryKey: ["recordings", teamId, "key"],
    queryFn: ({ signal }) => api<RecordingKeyState>(`${team(teamId)}/recording-key`, { signal }),
    enabled: Boolean(teamId),
    retry: (count, error) => !(error instanceof ApiError && error.status >= 400 && error.status < 500) && count < 2,
  })

export function putManualStart(teamId: string, manual_start: ManualStart) {
  return api<Schemas["RecordingSettings"]>(`${team(teamId)}/recording-settings`, {
    method: "PUT",
    headers: json,
    body: JSON.stringify({ manual_start }),
  })
}

// The server's JSON carries bytes as standard base64 (Go encoding/json); the vault worker speaks base64url.
const byteFields = new Set(["sharing_public_key", "sealed", "sealed_previous", "public_key", "signing_public_key", "rotation_signature"])

/** Rewrites every byte field (by name, at any depth) between standard base64 and unpadded base64url. */
export function convertBytes<T>(value: T, to: "url" | "std"): T {
  const convert = (text: string) =>
    to === "url"
      ? text.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")
      : text.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(text.length / 4) * 4, "=")
  const walk = (item: unknown, key?: string): unknown =>
    Array.isArray(item)
      ? item.map((entry) => walk(entry))
      : item && typeof item === "object"
        ? Object.fromEntries(Object.entries(item).map(([name, entry]) => [name, walk(entry, name)]))
        : typeof item === "string" && key && byteFields.has(key)
          ? convert(item)
          : item
  return walk(value) as T
}

/** One recording call to the vault worker, with byte fields converted both ways; `file` goes beside the JSON. */
export async function recordingVault<T>(request: { action: string } & Record<string, unknown>, file?: Uint8Array): Promise<T> {
  return convertBytes(await vaultCrypto<T>(convertBytes(request, "url"), file), "std")
}

/** Viewers a copy can be sealed for: they have a vault, and no copy of the current generation yet. */
export const waitingViewers = (state: RecordingKeyState) =>
  state.viewers.filter((viewer): viewer is Required<RecordingViewer> => Boolean(viewer.sharing_public_key) && !viewer.has_grant)

/**
 * Generation 1: the vault worker makes the key pair, seals its private half to every viewer that has a vault and
 * forgets it; only the public half and the sealed copies reach the server.
 */
export async function createRecordingKey(teamId: string, viewers: Required<RecordingViewer>[]) {
  const made = await recordingVault<{ key: RecordingKey; grants: RecordingGrant[] }>({
    action: "recording-key-create",
    team_id: teamId,
    viewers: viewers.map(({ account_id, sharing_public_key }) => ({ account_id, sharing_public_key })),
  })
  return api<RecordingKey>(`${team(teamId)}/recording-key`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ key: made.key, grants: made.grants }),
  })
}

/** Seals the caller's own copy (opened in the unlocked vault) for the viewers still waiting. */
export async function grantRecordingKey(teamId: string, state: RecordingKeyState) {
  if (!state.key || !state.my_key) throw new Error(recordingSentences.recording_key_pending)
  const viewers = waitingViewers(state)
  const sealed = await recordingVault<{ grants: RecordingGrant[] }>({
    action: "recording-key-grant",
    vault: await currentVault(),
    team_id: teamId,
    generation: state.my_key.generation,
    sealed: state.my_key.sealed,
    viewers: viewers.map(({ account_id, sharing_public_key }) => ({ account_id, sharing_public_key })),
  })
  return api<Schemas["RecordingGrantsResult"]>(`${team(teamId)}/recording-key/grants`, {
    method: "PUT",
    headers: json,
    body: JSON.stringify({ generation: state.key.generation, grants: sealed.grants }),
  })
}

// ---- Rules: ordinary policy rules allowing `record` or `recording_view` (shared/policy RecordingActions) ----

export type RecordingAction = "record" | "recording_view"

export type RecordingRule = { index: number; action: RecordingAction; src: string[]; dst: string[] }

/** The policy's rules that record or allow replay, one entry per action, in file order. */
export function recordingRules(text: string): RecordingRule[] {
  const rules = parsePolicy(text)?.rules ?? []
  return rules.flatMap((rule, index) =>
    (["record", "recording_view"] as const)
      .filter((action) => rule.allow?.includes(action))
      .map((action) => ({ index, action, src: rule.src ?? [], dst: rule.dst ?? [] })),
  )
}

/**
 * The policy text with one recording rule added or one action removed. A file with comments would lose them on
 * rewrite, so it returns undefined and the person edits the file on Access instead.
 */
export function editRecordingRules(
  text: string,
  change: { add: { action: RecordingAction; src: string; dst: string } } | { remove: RecordingRule },
): string | undefined {
  const file = parsePolicy(text)
  if (!file || hasComments(text)) return undefined
  const rules: PolicyRule[] = [...(file.rules ?? [])]
  if ("add" in change) rules.push({ src: [change.add.src], dst: [change.add.dst], allow: [change.add.action] })
  else {
    const rule = rules[change.remove.index]
    if (!rule) return undefined
    const allow = (rule.allow ?? []).filter((action) => action !== change.remove.action)
    if (allow.length === 0 && !rule.deny?.length) rules.splice(change.remove.index, 1)
    else rules[change.remove.index] = { ...rule, allow }
  }
  return JSON.stringify({ ...file, rules }, null, 2) + "\n"
}

/** "user:ana@example.test" → "ana@example.test", "tag:prod" → "prod", "*" → "Everyone". */
export function subjectLabel(entry: string) {
  if (entry === "*") return "Everyone"
  if (entry === "machine:*") return "All machines"
  const [kind, name] = [entry.slice(0, entry.indexOf(":")), entry.slice(entry.indexOf(":") + 1)]
  if (kind === "role") return { owner: "Owners", admin: "Owners and admins", member: "Every member" }[name] ?? name
  return kind === "user" || kind === "machine" || kind === "group" || kind === "tag" ? name : entry
}

// ---- Asciicast v2 (what a recording decrypts to) ----

export type CastEvent = { time: number; code: "o" | "r" | "m"; data: string }
export type Cast = { width: number; height: number; started: number; events: CastEvent[]; duration: number }

/** Parses asciicast v2 lines; unknown event codes and blank lines are skipped, a bad line throws. */
export function parseCast(text: string): Cast {
  const lines = text.split("\n").filter((line) => line.trim())
  const header = JSON.parse(lines[0] ?? "null") as { version?: number; width?: number; height?: number; timestamp?: number } | null
  if (!header || header.version !== 2 || !(Number(header.width) > 0) || !(Number(header.height) > 0))
    throw new Error("The recording's header is invalid.")
  const events: CastEvent[] = []
  for (const line of lines.slice(1)) {
    const [time, code, data] = JSON.parse(line) as [number, string, string]
    if (typeof time !== "number" || typeof data !== "string") throw new Error("The recording has an invalid event.")
    if (code === "o" || code === "r" || code === "m") events.push({ time, code, data })
  }
  return {
    width: Number(header.width),
    height: Number(header.height),
    started: Number(header.timestamp ?? 0) * 1000,
    events,
    duration: events.at(-1)?.time ?? 0,
  }
}

/** "1h 02m", "4m 05s", "12s". */
export function formatDuration(seconds: number) {
  const s = Math.max(0, Math.round(seconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (h) return `${h}h ${String(m).padStart(2, "0")}m`
  if (m) return `${m}m ${String(s % 60).padStart(2, "0")}s`
  return `${s}s`
}

/** "1:02:05" or "4:05" for the player's clock. */
export function formatClock(seconds: number) {
  const s = Math.max(0, Math.floor(seconds))
  const h = Math.floor(s / 3600)
  const mm = String(Math.floor((s % 3600) / 60)).padStart(h ? 2 : 1, "0")
  return `${h ? `${h}:` : ""}${mm}:${String(s % 60).padStart(2, "0")}`
}

/**
 * Reassembles the replay stream: binary frames whose start is their byte offset, carrying segment files each behind a
 * 4-byte big-endian length. Returns the segment files completed by each frame, in order.
 */
export class SegmentFramer {
  private buffer = new Uint8Array(0)
  private offset = 0

  push(start: number, data: Uint8Array): Uint8Array[] {
    if (start !== this.offset) throw new Error("The recording stream skipped or repeated bytes.")
    this.offset += data.byteLength
    const joined = new Uint8Array(this.buffer.byteLength + data.byteLength)
    joined.set(this.buffer)
    joined.set(data, this.buffer.byteLength)
    const files: Uint8Array[] = []
    let at = 0
    while (joined.byteLength - at >= 4) {
      const size = new DataView(joined.buffer, at, 4).getUint32(0)
      if (size === 0 || size > maxSegmentFile) throw new Error("The recording stream has an invalid segment.")
      if (joined.byteLength - at - 4 < size) break
      files.push(joined.slice(at + 4, at + 4 + size))
      at += 4 + size
    }
    this.buffer = joined.slice(at)
    return files
  }

  /** Whether a partial segment is still waiting for bytes. */
  get pending() {
    return this.buffer.byteLength > 0
  }
}

/** cli/internal/recording MaxSegmentFile: 4 MiB of asciicast plus 64 KiB. */
const maxSegmentFile = (4 << 20) + (64 << 10)
