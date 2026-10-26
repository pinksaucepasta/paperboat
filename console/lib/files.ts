"use client"

import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"
import { resourceId, type OperationOrResource } from "@/lib/tunnels"
import { uploadFile } from "@/lib/upload"

/*
 * Files: each machine's Paperboat Inbox (Downloads/Paperboat Inbox on the machine), where files
 * sent from the console, dropped into a terminal, sent with `pb send` or approved from a teammate
 * land. Paperboat keeps no file names, sizes or contents: the machine answers every call live
 * through the server, so an offline machine's Inbox is unavailable until it is back. Downloading
 * and deleting stay off (`inboxLive`) until the server and daemons that serve them are deployed.
 */

/** Which Inbox endpoints the deployed server answers. Flip one when its deploy is logged. */
export const inboxLive = { upload: false, content: true, remove: true, send: false, share: false } as const

export type InboxFile = {
  id: string
  name: string
  size: number
  received_at: string
}

/** `GET /v1/machines/{id}/inbox`: the machine's Inbox right now, newest first. */
export type InboxListing = {
  /** Files in the Inbox; the list holds the newest 200. */
  total: number
  items: InboxFile[]
}

/** A Team Inbox request as `GET /v1/team-inbox/requests` lists it (teaminbox.Request). */
export type InboxRequest = {
  request_id: string
  team_id: string
  sender_account: string
  recipient_account: string
  source_machine_id: string
  destination_machine_id: string
  files: { basename: string; size: number; sha256: string }[]
  status: "pending" | "approved" | "declined" | "expired" | "revoked" | "completed" | string
  /** After approval, how the transfer is going (File transfer modes, PLAN 3b); `status` is the approval. */
  transfer_state?: "sending" | "paused" | "done" | "failed"
  pause_reason?: "no_eligible_path" | "destination_offline" | "relay_quota_exhausted"
  transfer_mode?: "automatic" | "unlimited"
  total_bytes?: number
  committed_bytes?: number
  decision_generation: number
  expires_at: string
}

const inboxPath = (machineId: string) => `machines/${encodeURIComponent(machineId)}/inbox`
const filePath = (machineId: string, fileId: string) => `${inboxPath(machineId)}/${encodeURIComponent(fileId)}`

export const fileKeys = {
  /** Every Inbox listing (the workspace list and its batches); refreshed after a send or delete. */
  inboxes: ["inbox"] as const,
  requests: (workspace: string) => ["team-inbox", workspace] as const,
}

/** One batch of `GET /v1/inbox`: up to 50 machines' Inboxes read live by the server, merged. */
export type InboxBatch = {
  items: (InboxFile & { machine: { id: string; alias: string } })[]
  /** Counts for the machines this batch read, never a workspace total. */
  machines_checked: number
  files_on_checked_machines: number
  /** Machines whose listing held only their newest 200 files. */
  truncated: string[]
  unavailable: { machine_id: string; alias: string; reason: string }[]
  next_machine_cursor?: string
}

/**
 * The workspace's Inboxes, one batch of machines per page ("Load more machines"); `machine`
 * narrows it to one machine, `q` filters file names. The server reads each machine live and
 * caches a batch for 30 s, so nothing is fanned out from the browser.
 */
export const inboxListQuery = (workspace: string, q: string, machine?: string) =>
  infiniteQueryOptions({
    queryKey: [...fileKeys.inboxes, workspace, q, machine ?? ""],
    queryFn: ({ pageParam }) =>
      api<InboxBatch>(
        `inbox?${new URLSearchParams({
          workspace,
          ...(q && { q }),
          ...(machine && { machine_id: machine }),
          ...(pageParam && { machine_cursor: pageParam }),
        })}`,
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_machine_cursor,
    refetchInterval: 30_000,
  })

export const requestsQuery = (workspace: string) =>
  queryOptions({
    queryKey: fileKeys.requests(workspace),
    queryFn: async () =>
      (await api<{ requests: InboxRequest[] }>(`team-inbox/requests?workspace=${encodeURIComponent(workspace)}`))
        .requests,
    refetchInterval: 30_000,
  })

/** Sends one file to a machine's Inbox, streamed through with progress. */
export const sendToInbox = (machineId: string, file: File, onProgress: (fraction: number) => void) =>
  uploadFile<InboxFile>(`${inboxPath(machineId)}?${new URLSearchParams({ name: file.name })}`, file, onProgress)

/** Where the browser downloads a file from; the machine streams it as it's read. */
export const downloadUrl = (machineId: string, fileId: string) => `/api/v1/${filePath(machineId, fileId)}/content`

const post = <T>(path: string, body?: unknown) =>
  api<T>(path, {
    method: "POST",
    headers: { "idempotency-key": idempotencyKey(), "content-type": "application/json" },
    body: JSON.stringify(body ?? {}),
  })

export const deleteInboxFile = (machineId: string, fileId: string) =>
  api(filePath(machineId, fileId), { method: "DELETE", headers: { "idempotency-key": idempotencyKey() } })

/** Copies a file from one machine's Inbox to another's, machine to machine (what `pb send` does). */
export const sendToMachine = (machineId: string, fileId: string, destinationId: string) =>
  post(`${filePath(machineId, fileId)}/send`, { destination_machine_id: destinationId })

/** Serves an Inbox file at a preview link, as Share a file does, without uploading it again. */
export const shareInboxFile = async (
  machineId: string,
  fileId: string,
  input: { access: "public" | "private"; hours: number },
) =>
  resourceId(
    await post<OperationOrResource>(`${filePath(machineId, fileId)}/share`, {
      access_mode: input.access,
      expires_at: new Date(Date.now() + input.hours * 3_600_000).toISOString(),
    }),
  )

export const decideRequest = (request: InboxRequest, decision: "approve" | "decline") =>
  post(`team-inbox/requests/${encodeURIComponent(request.request_id)}/${decision}`, {
    expected_generation: request.decision_generation,
  })
