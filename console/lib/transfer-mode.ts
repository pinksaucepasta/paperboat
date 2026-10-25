"use client"

import { queryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"

/**
 * File transfer modes (PLAN 3b), turned on as Lane O's parts deploy: the account preference
 * (GET/PATCH /v1/me/preferences), then transfer progress on Team Inbox requests.
 */
export const transferModesLive = { preference: true, progress: false } as const

export type TransferMode = "automatic" | "unlimited"

/** The two choices as the console words them, one line each. */
export const transferModes: Record<TransferMode, { label: string; detail: string }> = {
  automatic: {
    label: "Automatic",
    detail: "Files up to 2 GB. Uses Paperboat’s relays when machines can’t connect directly.",
  },
  unlimited: {
    label: "Unlimited",
    detail: "Files of any size. Only over a direct connection or your own relays.",
  },
}

/** Why a transfer is paused (pause_reason), in plain words. */
export const pauseReasons: Record<string, string> = {
  no_eligible_path: "The machines can’t connect directly, and there is no relay of yours to use.",
  destination_offline: "The receiving machine is offline.",
  relay_quota_exhausted: "This workspace’s hosted bandwidth is used up.",
}

type Preferences = { file_transfer_mode: TransferMode }

/** The account's preferences (GET /v1/me/preferences); the CLI's transfers.mode is the same field. */
export const preferencesQuery = queryOptions({
  queryKey: ["me", "preferences"],
  queryFn: () => api<Preferences>("me/preferences"),
  enabled: transferModesLive.preference,
})

export const setTransferMode = (mode: TransferMode) =>
  api<Preferences>("me/preferences", {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ file_transfer_mode: mode }),
  })
