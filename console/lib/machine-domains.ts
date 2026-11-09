"use client"

import { clientConfigQuery, useApiQuery } from "@/lib/api"

/**
 * Machines answer as `<name>.<domain>` under Paperboat's local access domain (from the server's
 * client configuration). Your own domains serve tunnels and previews (Domains page); machine
 * addresses on them wait for the server to deliver them to machines (NOTES.md, "Machine domains").
 */
export type MachineDomains = { primary: string }

export function useMachineDomains(): MachineDomains {
  return { primary: useApiQuery(clientConfigQuery).data?.local_access_base_domain ?? "" }
}

/** Every address a machine name has; none until the domain is known. */
export function useMachineHostnames(alias: string): string[] {
  const { primary } = useMachineDomains()
  return primary ? [`${alias}.${primary}`] : []
}
