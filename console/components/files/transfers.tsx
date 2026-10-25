"use client"

import { meQuery, useApiQuery, useMachines, useWorkspace } from "@/lib/api"
import { requestsQuery, type InboxRequest } from "@/lib/files"
import { formatBytes } from "@/lib/format"
import { pauseReasons, transferModes, transferModesLive } from "@/lib/transfer-mode"

/**
 * Transfers in progress (approved Team Inbox requests that are sending, paused or failed), to or
 * from your machines, with how far each got. Paused ones say what they wait for.
 */
export function Transfers({ scope }: { scope?: string | null }) {
  const workspace = useWorkspace()
  const { data: me } = useApiQuery(meQuery)
  const machines = useMachines()
  const requests = useApiQuery({ ...requestsQuery(workspace?.id ?? ""), enabled: Boolean(workspace) && transferModesLive })
  const running = (requests.data ?? []).filter(
    (request) =>
      ["sending", "paused", "failed"].includes(request.state ?? "") &&
      (request.sender_account === me?.id || request.recipient_account === me?.id) &&
      (!scope || request.destination_machine_id === scope || request.source_machine_id === scope),
  )
  if (!transferModesLive || !running.length) return null
  const aliasOf = (id: string) => machines.data?.find((entry) => entry.id === id)?.alias ?? "a machine"
  return (
    <div className="flex flex-col gap-2">
      <h2 className="text-sm font-medium">Transfers</h2>
      <div className="flex flex-col divide-y divide-divider rounded-lg bg-surface px-4" data-transfers>
        {running.map((request) => (
          <TransferRow key={request.request_id} request={request} to={aliasOf(request.destination_machine_id)} />
        ))}
      </div>
    </div>
  )
}

function TransferRow({ request, to }: { request: InboxRequest; to: string }) {
  const [first, ...rest] = request.files
  const total = request.total_bytes ?? request.files.reduce((sum, file) => sum + file.size, 0)
  const done = request.committed_bytes ?? 0
  const mode = request.transfer_mode ? transferModes[request.transfer_mode]?.label : undefined
  return (
    <div className="flex flex-col gap-1 py-3 text-sm">
      <span className="truncate">
        <span className="font-medium">{first?.basename}</span>
        {rest.length > 0 && ` and ${rest.length} more`} <span className="text-muted-foreground">to {to}</span>
      </span>
      <span className="text-xs text-muted-foreground">
        {formatBytes(done)} of {formatBytes(total)}
        {mode && ` · ${mode}`}
        {request.state === "sending" && " · Sending"}
        {request.state === "failed" && " · Failed"}
      </span>
      {request.state === "paused" && (
        <span className="text-xs">
          Waiting for a direct connection or your relay.{" "}
          <span className="text-muted-foreground">{pauseReasons[request.pause_reason ?? ""] ?? ""}</span>
        </span>
      )}
    </div>
  )
}
