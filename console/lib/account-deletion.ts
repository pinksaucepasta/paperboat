"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, send } from "@/lib/api"

/** Account deletion (F2-09, server 2026.10.23.71). */
export const accountDeletionLive = true

type Team = { slug: string; name: string }

/** What deleting the account removes, from GET /v1/account/deletion-preview (Lane B's contract). */
export type DeletionPreview = {
  account_email: string
  machines: number
  env_vaults: number
  previews: number
  tunnels: number
  config_sync: boolean
  inbox_items: number
  api_tokens: number
  subscription: { plan: string; period_end: string | null } | null
  /** Teams where you are the only member: deleted with the account. */
  teams_deleted: Team[]
  /** Teams you own that have other members: they block the deletion. */
  teams_blocking: (Team & { members: number })[]
  reauthenticated_at: string | null
  /** The last sign-in is more than five minutes old: sign in again before deleting. */
  reauthentication_required: boolean
}

export const deletionPreviewQuery = queryOptions({
  queryKey: ["account", "deletion-preview"],
  queryFn: () => api<DeletionPreview>("account/deletion-preview"),
  enabled: accountDeletionLive,
})

/** A refusal of DELETE /v1/account, worded for the page. */
export class DeletionError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly teams?: DeletionPreview["teams_blocking"],
  ) {
    super(message)
  }
}

const sentences: Record<string, string> = {
  confirmation_mismatch: "That isn’t your account’s email. Type it exactly as shown.",
  account_owns_team: "You still own teams with other members. Hand them over or delete them first.",
  reauthentication_required: "Sign in again to delete your account.",
}

/**
 * Deletes the account (204; the server ends the session). Read directly: a 401 here means
 * "sign in again", not an ended session (api() would treat it as signed out).
 */
export async function deleteAccount(confirmEmail: string): Promise<void> {
  const response = await send("/api/v1/account", {
    method: "DELETE",
    headers: { "content-type": "application/json", accept: "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify({ confirm_email: confirmEmail }),
  })
  if (response.ok) return
  const body = (await response.json().catch(() => null)) as {
    error?: { code?: string; message?: string; details?: { teams?: DeletionPreview["teams_blocking"] } }
  } | null
  const code = body?.error?.code ?? "unavailable"
  throw new DeletionError(
    code,
    sentences[code] ?? body?.error?.message ?? "Paperboat is temporarily unavailable. Try again.",
    body?.error?.details?.teams,
  )
}
