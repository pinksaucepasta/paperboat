"use client"

import Link from "next/link"
import { use, useState } from "react"
import { PagerBar } from "@/components/data-table"
import { pageSize } from "@/lib/paging"
import { ErrorBlock } from "@/components/error-block"
import { MachinesOf } from "@/components/machines-of"
import { TeamActions } from "@/components/account-actions"
import { PlanGrant } from "@/components/plan-grant"
import { BanState } from "@/components/ban-state"
import { Copyable, StatusBadge } from "@/components/filters"
import { Facts, ObjectTitle, Page, Panel } from "@/components/page"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"

type Team = Data<"/v1/admin/teams/{team_id}">

const userLink = (id: string) => (
  <Link className="font-mono text-xs underline-offset-2 hover:underline" href={`/users/${encodeURIComponent(id)}`}>
    {id}
  </Link>
)

export default function TeamPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params)
  // Keyed by id: another team starts again at its first page of members.
  return <TeamView key={id} id={id} />
}

function TeamView({ id }: { id: string }) {
  // Members are paged with the team: cursors[i] opens page i+1 (Previous goes back to a cursor already seen).
  const [cursors, setCursors] = useState([""])
  const page = cursors.length
  const after = cursors[page - 1]
  const team = useApiQuery<Team>(["team", id, after], `/v1/admin/teams/${encodeURIComponent(id)}?limit=${pageSize}${after ? `&after=${encodeURIComponent(after)}` : ""}`)
  const t = team.data
  return (
    <Page parent={{ href: "/teams", title: "Teams" }} title={id}>
      {team.isError ? (
        <ErrorBlock what="this team" error={team.error} retry={() => team.refetch()} />
      ) : !t ? (
        <div className="flex flex-col gap-8">
          <Skeleton className="my-1 h-6 w-64" />
          <Skeleton className="h-40 w-full" />
        </div>
      ) : (
        <>
          <ObjectTitle detail={<BanState banned={t.suspended} state="active" reason={t.ban_reason} />}>{t.id}</ObjectTitle>
          <Facts
            rows={[
              ["Team id", <Copyable key="i" value={t.id} />],
              ["Owner", userLink(t.owner_account)],
              ["Billing", t.billing_workspace_id ? <Link key="b" className="font-medium underline-offset-2 hover:underline" href={`/billing?workspace=${encodeURIComponent(t.billing_workspace_id)}`}>{t.billing_state ?? t.billing_workspace_id}</Link> : "—"],
              ["Generation", t.generation],
            ]}
          />
          <Panel title="Members">
            <ul className="text-sm">
              {t.members.map((m) => (
                <li key={m.account_id} className="flex h-12 items-center gap-3 border-b border-divider">
                  {userLink(m.account_id)}
                  <span className="text-muted-foreground inline-block first-letter:uppercase">{m.role}</span>
                  {!m.active && <StatusBadge state="revoked" />}
                </li>
              ))}
            </ul>
            <PagerBar
              page={page}
              count={t.members.length}
              total={t.members_total}
              hasNext={Boolean(t.members_next_after)}
              go={(next) => setCursors(next > page && t.members_next_after ? [...cursors, t.members_next_after] : cursors.slice(0, next))}
            />
          </Panel>
          <PlanGrant scope="team" id={t.id} />
          <Panel title="Machines">
            <MachinesOf owner={{ team: t.id }} />
          </Panel>
          <TeamActions id={t.id} suspended={t.suspended} reason={t.ban_reason} generation={t.generation} />
        </>
      )}
    </Page>
  )
}
