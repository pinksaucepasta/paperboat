"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, apiAction, type Data } from "@/lib/api"

/*
 * Port names (GET/POST /v1/port-names): a name for a port that opens in a browser on every signed-in
 * computer. A name never grants access; it only names a port the viewer may already open.
 */

export type PortName = Data<"/v1/port-names">["items"][number]
export type PortNameScope = PortName["scope"]
export type FrontDesk = Data<"/v1/machines/{machine_id}/front-desk">

const query = (workspace: string) => `?workspace=${encodeURIComponent(workspace)}`

export const portNamesQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["port-names", workspace],
    queryFn: async ({ signal }) => (await api<{ items: PortName[] }>(`port-names${query(workspace)}`, { signal })).items,
  })

export const setPortName = (
  workspace: string,
  body: { scope: PortNameScope; name: string; port: number; machine_id?: string },
) =>
  api<PortName>(`port-names${query(workspace)}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  })

export const removePortName = (workspace: string, id: string) =>
  apiAction(`port-names/${encodeURIComponent(id)}${query(workspace)}`, { method: "DELETE" })

export const frontDeskQuery = (machineId: string) =>
  queryOptions({
    queryKey: ["front-desk", machineId],
    queryFn: ({ signal }) => api<FrontDesk>(`machines/${encodeURIComponent(machineId)}/front-desk`, { signal }),
  })

export const saveFrontDesk = (machineId: string, desk: FrontDesk) =>
  api<FrontDesk>(`machines/${encodeURIComponent(machineId)}/front-desk`, {
    method: "PUT",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(desk),
  })

/** The address a name opens, as people type it into a browser. */
export function portNameAddress(name: PortName, domain: string): string {
  if (name.scope === "top_level") return `${name.name}.${domain}`
  return `${name.name}.${(name.scope === "machine" && name.machine_alias) || "<machine>"}.${domain}`
}

/** One lowercase DNS label that is not a number (the server's rule). */
export const portNamePattern = /^(?!\d+$)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

export const validPort = (value: number) => Number.isInteger(value) && value >= 1 && value <= 65535
