"use client"

import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { relaysQuery, useApiQuery } from "@/lib/api"
import { poolQuery, poolServersQuery, refreshNetwork, savePool, type scopeOf } from "@/lib/network"
import { RegionLine, RegionTable } from "@/components/network/region-table"
import { SectionHeading } from "@/components/section"
import { Switch } from "@/components/ui/switch"
import { LoadError } from "@/components/load-error"
import { errorToast } from "@/components/error-reference"

/**
 * Paperboat's relay regions. With a relay of your own, a switch keeps relayed traffic on your relays
 * only (the account's relay pool); your own relays are listed under Your regions.
 */
export function RelaysSection({ scope }: { scope: ReturnType<typeof scopeOf> }) {
  const queryClient = useQueryClient()
  const relays = useApiQuery(relaysQuery)
  const pool = useApiQuery(poolQuery("relay", scope))
  const own = useInfiniteQuery(poolServersQuery("relay", scope, ""))
  const ownIds = (own.data?.pages.flatMap((page) => page.items) ?? []).map((server) => server.installation_id)
  const only = pool.data?.mode === "self-hosted-only"
  const save = useMutation({
    mutationFn: (on: boolean) =>
      savePool(
        "relay",
        scope,
        on
          ? { mode: "self-hosted-only", installation_ids: ownIds.slice(0, 32) }
          : { mode: "mixed", installation_ids: [] },
      ),
    onSuccess: async (_, on) => {
      await refreshNetwork(queryClient)
      toast.success(on ? "Relayed traffic uses only your relays" : "Relayed traffic can use Paperboat’s relays")
    },
    onError: (error) => errorToast("Couldn’t change relays", error),
  })
  // Ready regions first.
  const rows = [...(relays.data ?? [])].sort(
    (a, b) => Number(b.status === "ready") - Number(a.status === "ready") || a.name.localeCompare(b.name),
  )
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-col gap-1">
        <SectionHeading
          title="Relays"
          action={
            ownIds.length > 0 &&
            pool.data && (
              <label className="flex items-center gap-2 text-sm">
                Use only my relays
                <Switch checked={only} disabled={save.isPending} onCheckedChange={(on) => save.mutate(on)} />
              </label>
            )
          }
        />
        <p className="text-sm text-muted-foreground">
          Used only when two machines can’t connect directly. Paperboat picks the fastest one for each connection.
        </p>
      </div>
      <RegionTable loading={relays.isPending} rows={2}>
        {rows.map((relay) => (
          <RegionLine key={relay.id} name={relay.name} status={relay.status} />
        ))}
      </RegionTable>
      {relays.isError && <LoadError what="relays" error={relays.error} retry={() => relays.refetch()} />}
    </section>
  )
}
