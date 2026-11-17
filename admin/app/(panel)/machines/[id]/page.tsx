"use client"

import Link from "next/link"
import { use } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { Facts, Page } from "@/components/page"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"

type Machine = Data<"/v1/admin/machines/{machine_id}">

export default function MachinePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params)
  const machine = useApiQuery<Machine>(["machine", id], `/v1/admin/machines/${encodeURIComponent(id)}`)
  const m = machine.data
  const when = (iso?: string | null) => (iso ? <Time iso={iso} /> : "—")
  return (
    <Page title={m?.alias ?? "Machine"}>
      {machine.isError ? (
        <ErrorBlock what="this machine" error={machine.error} retry={() => machine.refetch()} />
      ) : !m ? (
        <Skeleton className="h-60 w-full" />
      ) : (
        <Facts
          rows={[
            ["Name", m.alias],
            ["Machine id", <Mono key="i">{m.id}</Mono>],
            ["State", <span key="s" className="flex gap-1"><StatusBadge state={m.state} />{m.deleted_at ? <StatusBadge state="deleted" /> : m.stale ? <StatusBadge state="stale" /> : m.ready ? <StatusBadge state="ready" /> : null}</span>],
            ["Owner account", <Link key="a" className="underline" href={`/users/${encodeURIComponent(m.account_id)}`}><Mono>{m.account_id}</Mono></Link>],
            ["Owner team", m.team_id ? <Link key="t" className="underline" href={`/teams/${encodeURIComponent(m.team_id)}`}><Mono>{m.team_id}</Mono></Link> : "—"],
            ["Platform", `${m.platform}/${m.architecture}`],
            ["Host service", m.host_service_version ?? "—"],
            ["Connector", m.connector_state],
            ["Update health", m.update_health],
            ["Capabilities", m.observed_capabilities.join(", ") || "none observed"],
            ["Install generation", m.installation_generation],
            ["Last seen", when(m.last_seen_at)],
            ["Enrolled", when(m.enrolled_at)],
            ["Revoked", when(m.revoked_at)],
            ["Removed", when(m.deleted_at)],
            ["Created", when(m.created_at)],
          ]}
        />
      )}
    </Page>
  )
}
