"use client"

import Link from "next/link"
import { use } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Copyable, StatusBadge } from "@/components/filters"
import { Facts, ObjectTitle, Page, Panel } from "@/components/page"
import { Skeleton } from "@/components/ui/skeleton"
import { Time } from "@/components/time"
import { useApiQuery, type Data } from "@/lib/api"
import { MachinesOf } from "@/components/machines-of"
import { UserActions } from "@/components/account-actions"
import { PlanGrant } from "@/components/plan-grant"

type Account = Data<"/v1/admin/accounts/{account_id}">

export default function UserPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params)
  const account = useApiQuery<Account>(["account", id], `/v1/admin/accounts/${encodeURIComponent(id)}`)
  const a = account.data
  return (
    <Page parent={{ href: "/users", title: "Users" }} title={a?.primary_email ?? <Skeleton className="my-0.5 h-4 w-40" />}>
      {account.isError ? (
        <ErrorBlock what="this user" error={account.error} retry={() => account.refetch()} />
      ) : !a ? (
        <div className="flex flex-col gap-8">
          <div className="flex flex-col gap-2">
            <Skeleton className="my-1 h-6 w-64" />
            <Skeleton className="my-0.5 h-4 w-48" />
          </div>
          <Skeleton className="h-40 w-full" />
        </div>
      ) : (
        <>
          <ObjectTitle
            detail={
              <>
                <StatusBadge state={a.status} />
                {a.display_name && <span>{a.display_name}</span>}
                <span>
                  Joined <Time iso={a.created_at} />
                </span>
              </>
            }
          >
            {a.primary_email}
          </ObjectTitle>
          <Facts
            rows={[
              ["Account id", <Copyable key="i" value={a.id} />],
              ["Billing", a.billing_workspace_id ? <Link key="b" className="font-mono text-xs underline-offset-2 hover:underline" href={`/billing?workspace=${encodeURIComponent(a.billing_workspace_id)}`}>{a.billing_workspace_id}</Link> : "—"],
              ["Version", a.version],
            ]}
          />
          <PlanGrant scope="personal" id={a.id} />
          <Panel title="Machines">
            <MachinesOf owner={{ account: a.id }} />
          </Panel>
          <UserActions id={a.id} email={a.primary_email} status={a.status} version={a.version} />
        </>
      )}
    </Page>
  )
}
