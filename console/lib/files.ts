"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"
import { resourceId, type OperationOrResource } from "@/lib/tunnels"
import { uploadFile } from "@/lib/upload"

/*
 * Files: each machine's Paperboat Inbox (Downloads/Paperboat Inbox on the machine), where files
 * sent from the console, dropped into a terminal, sent with `pb send` or approved from a teammate
 * land. Paperboat keeps no file contents: listing, downloading and deleting are answered live by
 * the machine through the server, so an offline machine's Inbox can't be read until it's back.
 * The Inbox endpoints are the console's side of a planned API (NOTES.md); Team Inbox requests
 * (`/v1/team-inbox/requests`) exist today.
 */

export type InboxFile = {
  id: string
  name: string
  size: number
  content_type?: string
  received_at: string
  /** Where it came from: the console, another machine (`pb send`), a terminal drop, or a teammate. */
  source: { kind: "console" | "machine" | "terminal" | "teammate"; machine_id?: string; name?: string }
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
  decision_generation: number
  expires_at: string
}

const inboxPath = (machineId: string) => `machines/${encodeURIComponent(machineId)}/inbox`
const filePath = (machineId: string, fileId: string) => `${inboxPath(machineId)}/${encodeURIComponent(fileId)}`

export const fileKeys = {
  inbox: (machineId: string) => ["inbox", machineId] as const,
  requests: (workspace: string) => ["team-inbox", workspace] as const,
}

export const inboxQuery = (machineId: string) =>
  queryOptions({
    queryKey: fileKeys.inbox(machineId),
    queryFn: async () => (await api<{ items: InboxFile[] }>(inboxPath(machineId))).items,
    refetchInterval: 15_000,
    // An unreachable machine answers quickly; say so instead of retrying for seconds.
    retry: 1,
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
