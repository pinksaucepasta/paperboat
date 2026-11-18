"use client"

import Link from "next/link"
import { use } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { Facts, Page, Panel } from "@/components/page"
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
  return (
    <Page title={account.data?.primary_email ?? "User"}>
      {account.isError ? (
        <ErrorBlock what="this user" error={account.error} retry={() => account.refetch()} />
      ) : !account.data ? (
        <Skeleton className="h-40 w-full" />
      ) : (
        <>
          <Facts
            rows={[
              ["Email", account.data.primary_email],
              ["Name", account.data.display_name],
              ["Status", <StatusBadge key="s" state={account.data.status} />],
              ["Account id", <Mono key="i">{account.data.id}</Mono>],
              ["Version", account.data.version],
              ["Billing workspace", account.data.billing_workspace_id ? <Link key="b" className="underline" href={`/billing?workspace=${encodeURIComponent(account.data.billing_workspace_id)}`}><Mono>{account.data.billing_workspace_id}</Mono></Link> : "—"],
              ["Created", <Time key="c" iso={account.data.created_at} />],
            ]}
          />
          <UserActions id={account.data.id} email={account.data.primary_email} status={account.data.status} version={account.data.version} />
          <PlanGrant scope="personal" id={account.data.id} />
          <Panel title="Machines">
            <MachinesOf owner={{ account: account.data.id }} />
          </Panel>
        </>
      )}
    </Page>
  )
}
