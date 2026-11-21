"use client"

import { StatusDot } from "@/components/machine-status"
import { TableRowsSkeleton } from "@/components/skeletons"

export const regionStatus = { ready: "Ready", draining: "Being replaced", unavailable: "Not reachable" } as const

/** Latency in ms, blank without a fresh measurement from your machines. */
export const latency = (ms?: number) => (ms === undefined ? "" : `${ms} ms`)

/** One row of the Relays or Edges table: (its role,) the region, whether it's ready, (latency). */
export function RegionLine({
  role,
  name,
  status,
  rtt,
  showLatency,
}: {
  role?: string
  name: React.ReactNode
  status: keyof typeof regionStatus
  rtt?: number
  showLatency?: boolean
}) {
  return (
    <tr className="[&>td]:h-14 [&>td]:px-3">
      {role && <td className="truncate font-medium">{role}</td>}
      <td className="truncate">{name}</td>
      <td>
        <span className="flex items-center gap-2 text-muted-foreground">
          <StatusDot online={status === "ready"} />
          {regionStatus[status]}
        </span>
      </td>
      {showLatency && <td className="text-right font-mono text-code text-muted-foreground">{latency(rtt)}</td>}
    </tr>
  )
}

/** The Relays and Edges tables: (Role,) Region, Status, (Latency). Regions only: no node, host or operator. */
export function RegionTable({
  roles,
  showLatency,
  loading,
  rows,
  children,
}: {
  roles?: boolean
  showLatency?: boolean
  loading: boolean
  rows: number
  children: React.ReactNode
}) {
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        {roles && <col className="w-[22%]" />}
        <col />
        <col className="w-[26%]" />
        {showLatency && <col className="w-[14%]" />}
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          {roles && <th>Role</th>}
          <th>Region</th>
          <th>Status</th>
          {showLatency && <th className="text-right">Latency</th>}
        </tr>
      </thead>
      <tbody className="divide-y divide-divider border-b border-divider">
        {loading ? (
          <TableRowsSkeleton
            rows={rows}
            columns={[
              roles ? { bar: "w-24" } : false,
              { bar: "w-24" },
              { bar: "w-14", dot: true },
              showLatency ? { className: "text-right", bar: "w-10" } : false,
            ]}
          />
        ) : (
          children
        )}
      </tbody>
    </table>
  )
}
