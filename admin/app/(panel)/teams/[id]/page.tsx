"use client"

import Link from "next/link"
import { use } from "react"
import { ErrorBlock } from "@/components/error-block"
import { MachinesOf } from "@/components/machines-of"
import { Mono, StatusBadge } from "@/components/filters"
import { Facts, Page, Panel } from "@/components/page"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"

type Team = Data<"/v1/admin/teams/{team_id}">

export default function TeamPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params)
  const team = useApiQuery<Team>(["team", id], `/v1/admin/teams/${encodeURIComponent(id)}`)
  const t = team.data
  return (
    <Page title="Team">
      {team.isError ? (
        <ErrorBlock what="this team" error={team.error} retry={() => team.refetch()} />
      ) : !t ? (
        <Skeleton className="h-40 w-full" />
      ) : (
        <>
          <Facts
            rows={[
              ["Team id", <Mono key="i">{t.id}</Mono>],
              ["Owner", <Link key="o" className="underline" href={`/users/${encodeURIComponent(t.owner_account)}`}><Mono>{t.owner_account}</Mono></Link>],
              ["Status", <StatusBadge key="s" state={t.suspended ? "suspended" : "active"} />],
              ["Generation", t.generation],
              ["Billing", t.billing_workspace_id ? <Link key="b" className="underline" href={`/billing?workspace=${encodeURIComponent(t.billing_workspace_id)}`}>{t.billing_state ?? t.billing_workspace_id}</Link> : "—"],
            ]}
          />
          <Panel title={`Members${t.members_truncated ? " (first page)" : ""}`}>
            <ul className="border-t border-divider text-sm">
              {t.members.map((m) => (
                <li key={m.account_id} className="flex items-center gap-3 border-b border-divider py-2">
                  <Link className="underline" href={`/users/${encodeURIComponent(m.account_id)}`}><Mono>{m.account_id}</Mono></Link>
                  <span className="text-muted-foreground">{m.role}</span>
                  {!m.active && <StatusBadge state="revoked" />}
                </li>
              ))}
            </ul>
          </Panel>
          <Panel title="Machines">
            <MachinesOf owner={{ team: t.id }} />
          </Panel>
        </>
      )}
    </Page>
  )
}
