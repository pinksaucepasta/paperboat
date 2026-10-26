"use client"

import { keepPreviousData, queryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { cursorPage } from "@/lib/cursor-pages"
import type { TunnelDomain } from "@/lib/tunnels"

/**
 * One hostname a tunnel or a live preview publishes, as GET /v1/domains lists it (Lane Q, PQ-04
 * C2): the same view as the owner's own domain list, plus who owns it.
 */
export type DomainItem = {
  kind: "tunnel" | "preview"
  domain: TunnelDomain & {
    tunnel_id?: string
    preview_id?: string
    /** The tunnel's name, for kind tunnel. */
    owner_name?: string
    /** The preview's address, for kind preview. */
    endpoint?: string
  }
}

type Kind = "tunnel" | "preview"

const params = (workspace: string, q: string, kind?: Kind) => ({
  workspace,
  ...(q && { q }),
  ...(kind && { kind }),
})

/** One page of the workspace's hostnames in name order, searched and counted by the server. */
export const domainsPageQuery = (options: { workspace: string; q: string; page: number; kind?: Kind }) =>
  queryOptions({
    queryKey: ["domains", options.workspace, "page", options],
    queryFn: () => cursorPage<DomainItem>("domains", params(options.workspace, options.q, options.kind), options.page),
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
  })

/** How many tunnel and preview hostnames use a domain name (the server matches it as a substring). */
export const domainUsesQuery = (workspace: string, name: string) =>
  queryOptions({
    queryKey: ["domains", workspace, "uses", name],
    queryFn: async () => {
      const total = async (kind: Kind) =>
        (
          await api<{ total: number }>(
            `domains?${new URLSearchParams({ ...params(workspace, name, kind), limit: "1" })}`,
          )
        ).total
      const [tunnels, previews] = await Promise.all([total("tunnel"), total("preview")])
      return { tunnels, previews }
    },
    refetchInterval: 30_000,
  })
