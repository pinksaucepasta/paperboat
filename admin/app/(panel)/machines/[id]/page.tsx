"use client"

import Link from "next/link"
import { use } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Copyable, StatusBadge } from "@/components/filters"
import { Facts, ObjectTitle, Page } from "@/components/page"
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
    <Page parent={{ href: "/machines", title: "Machines" }} title={m?.alias ?? id}>
      {machine.isError ? (
        <ErrorBlock what="this machine" error={machine.error} retry={() => machine.refetch()} />
      ) : !m ? (
        <Skeleton className="h-60 w-full" />
      ) : (
        <>
        <ObjectTitle
          detail={
            <>
              <StatusBadge state={m.deleted_at ? "removed" : m.stale ? "stale" : m.state} />
              <span>{`${m.platform}/${m.architecture}`}</span>
              {m.last_seen_at && (
                <span>
                  Seen <Time iso={m.last_seen_at} relative />
                </span>
              )}
            </>
          }
        >
          {m.alias}
        </ObjectTitle>
        <Facts
          rows={[
            ["Machine id", <Copyable key="i" value={m.id} />],
            ["Owner", <Link key="a" className="font-mono text-xs underline-offset-2 hover:underline" href={`/users/${encodeURIComponent(m.account_id)}`}>{m.account_id}</Link>],
            ["Team", m.team_id ? <Link key="t" className="font-mono text-xs underline-offset-2 hover:underline" href={`/teams/${encodeURIComponent(m.team_id)}`}>{m.team_id}</Link> : "—"],
            ["Ready", m.ready ? "Yes" : "No"],
            ["Host service", m.host_service_version ?? "—"],
            ["Connector", m.connector_state],
            ["Update health", m.update_health],
            ["Capabilities", m.observed_capabilities.join(", ") || "None"],
            ["Install generation", m.installation_generation],
            ["Enrolled", when(m.enrolled_at)],
            ["Revoked", when(m.revoked_at)],
            ["Removed", when(m.deleted_at)],
            ["Created", when(m.created_at)],
          ]}
        />
        </>
      )}
    </Page>
  )
}
