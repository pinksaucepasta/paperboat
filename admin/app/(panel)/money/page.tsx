"use client"

import { useMemo, useState } from "react"
import { Failures, Toggle, Trend } from "@/components/analytics"
import { DataTable } from "@/components/data-table"
import { ErrorBlock } from "@/components/error-block"
import { Mono } from "@/components/filters"
import { ListCard, MoneyStat, moneyFormatter, type MoneyReport } from "@/components/money"
import { Facts, Page, Panel } from "@/components/page"
import { Tabs } from "@/components/tabs"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { ApiError, useApiQuery, type Data } from "@/lib/api"
import { notConfiguredLine } from "@/lib/integrations"
import { useCursorList } from "@/lib/paging"

type Days = "7" | "30"
type View = "money" | "events"

export default function MoneyPage() {
  const [view, setView] = useState<View>("money")
  const [days, setDays] = useState<Days>("30")
  return (
    <Page title="Money">
      <Tabs value={view} onChange={setView} options={[["money", "Revenue"], ["events", "Stripe events"]]} />
      {view === "money" ? (
        <>
          <div className="flex justify-end">
            <Toggle values={[["7", "7 days"], ["30", "30 days"]]} value={days} onChange={setDays} />
          </div>
          <Report days={days} />
        </>
      ) : (
        <Events />
      )}
    </Page>
  )
}

function Unavailable({ error, retry }: { error: Error; retry: () => void }) {
  if (error instanceof ApiError && error.code === "money_not_configured") {
    return <p className="py-12 text-center text-sm text-muted-foreground" data-money-state="not-configured">{notConfiguredLine.money_not_configured}</p>
  }
  return <ErrorBlock what="the money report" error={error} retry={retry} />
}

const percent = (n: number) => `${n}%`

function Report({ days }: { days: Days }) {
  const q = useApiQuery<MoneyReport>(["money", days], `/v1/admin/money?days=${days}`, 120_000)
  if (q.isError) return <Unavailable error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-96 w-full" />
  const r = q.data
  const money = moneyFormatter(r.currency)
  const m = r.movement
  const vs = (p: { value: number; previous: number }) => ({ now: p.value, before: p.previous })
  const flow = (f: typeof m.new) => (f.previous_count == null ? undefined : { now: f.count, before: f.previous_count })
  const label = days === "7" ? "week" : "month"
  return (
    <div className="flex flex-col gap-8" data-money-state="ready">
      {(r.partial || r.skipped_currency > 0) && (
        <p className="text-sm text-muted-foreground" data-money-note>
          {r.partial && "More data than this page reads: totals are a floor. "}
          {r.skipped_currency > 0 && `${r.skipped_currency} subscription${r.skipped_currency === 1 ? "" : "s"} in other currencies are left out. `}
        </p>
      )}
      <Failures errors={r.errors} provider="Stripe" />
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <MoneyStat label="MRR" value={money(r.mrr.value)} previous={vs(r.mrr)} note={`vs ${days} days ago`} />
        <MoneyStat label="ARR" value={money(r.arr.value)} previous={vs(r.arr)} />
        <MoneyStat label="Paying subscribers" value={String(r.subscribers.value)} previous={vs(r.subscribers)} note={r.ending > 0 ? `${r.ending} ending` : undefined} />
        {r.cash && <MoneyStat label={`Net revenue, ${days} days`} value={money(r.cash.net.value)} previous={vs(r.cash.net)} />}
      </div>
      <Trend title={`MRR, last ${days} days`} days={r.trend.map((p) => ({ day: p.day, visitors: p.mrr }))} format={money} />

      <Panel title={`This ${label}`}>
        <div className="grid grid-cols-2 gap-3 md:grid-cols-5" data-money-movement>
          <MoneyStat label="New" value={String(m.new.count)} previous={flow(m.new)} note={m.new.mrr > 0 ? `+${money(m.new.mrr)} MRR` : undefined} />
          <MoneyStat label="Upgraded" value={String(m.upgraded.count)} previous={flow(m.upgraded)} note={m.upgraded.mrr > 0 ? `+${money(m.upgraded.mrr)} MRR` : undefined} />
          <MoneyStat label="Downgraded" value={String(m.downgraded.count)} previous={flow(m.downgraded)} worseUp note={m.downgraded.mrr > 0 ? `−${money(m.downgraded.mrr)} MRR` : undefined} />
          <MoneyStat label="Churned" value={String(m.churned.count)} previous={flow(m.churned)} worseUp note={m.churned.mrr > 0 ? `−${money(m.churned.mrr)} MRR` : undefined} />
          <MoneyStat label="Churn rate" value={percent(m.logo_churn_percent)} previous={{ now: m.logo_churn_percent, before: m.previous_logo_churn_percent }} worseUp points note={`${percent(m.revenue_churn_percent)} of MRR`} />
        </div>
      </Panel>

      {r.cash && (
        <Panel title="Revenue collected">
          <div className="grid grid-cols-2 gap-3 md:grid-cols-4" data-money-cash>
            <MoneyStat label="Collected" value={money(r.cash.collected.value)} previous={vs(r.cash.collected)} />
            <MoneyStat label="Refunds" value={money(r.cash.refunds.value)} previous={vs(r.cash.refunds)} worseUp />
            <MoneyStat label="Net" value={money(r.cash.net.value)} previous={vs(r.cash.net)} />
            <MoneyStat label="Failed payments" value={String(r.cash.failed_payments.value)} previous={vs(r.cash.failed_payments)} worseUp />
          </div>
        </Panel>
      )}

      <div className="grid gap-8 lg:grid-cols-2">
        <ListCard
          title="Failed payments"
          kind="past_due"
          total={r.dunning.count}
          rows={r.dunning.rows}
          money={money}
          empty="No one is behind on payments."
          summary={r.dunning.count > 0 ? `${r.dunning.count} behind · ${money(r.dunning.at_risk_mrr)} MRR at risk` : undefined}
        />
        <ListCard title="Top customers" kind="customers" total={r.customers.total} rows={r.customers.rows} money={money} empty="No paying customers yet." />
        <ListCard title="Renewing in the next 30 days" kind="renewals" total={r.renewals.total} rows={r.renewals.rows} money={money} empty="No renewals due." />
        <Panel title="Plans">
          <ul className="text-sm" data-money-plans>
            {r.plans.slice(0, 5).map((p) => (
              <li key={p.plan} className="flex items-center justify-between gap-3 border-b border-divider py-2.5">
                <span className="min-w-0 truncate">{p.plan}</span>
                <span className="tabular-nums">
                  {money(p.mrr)}
                  <span className="ml-2 text-xs text-muted-foreground">{p.subscriptions} subscription{p.subscriptions === 1 ? "" : "s"} · {p.seats} seat{p.seats === 1 ? "" : "s"}</span>
                </span>
              </li>
            ))}
            {r.plans.length === 0 && <li className="py-6 text-center text-muted-foreground">No paying subscriptions.</li>}
          </ul>
          {r.plans.length > 5 && <p className="text-xs text-muted-foreground">and {r.plans.length - 5} more plans</p>}
        </Panel>
        {r.balance && (
          <Panel title="Stripe balance">
            <Facts
              rows={[
                ["Available", money(r.balance.available)],
                ["Pending", money(r.balance.pending)],
                ["Next payout", r.balance.next_payout ? <span key="p">{money(r.balance.next_payout.amount)} · <Time iso={new Date(r.balance.next_payout.arrival * 1000).toISOString()} /></span> : "None on its way"],
              ]}
            />
          </Panel>
        )}
      </div>
      <p className="text-xs text-muted-foreground">
        MRR is Stripe’s definition: active and past-due subscriptions normalised to a month, after running discounts; one-time charges and tax are not in it. Updated <Time iso={r.observed_at} relative />.
      </p>
    </div>
  )
}

function Events() {
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Data<"/v1/admin/money/events">>("/v1/admin/money/events", filters, "cursor")
  return (
    <DataTable
      what="Stripe events"
      list={list}
      rows={(d) => d.events}
      rowKey={(e) => e.id}
      empty="No events."
      columns={[
        { header: "Event", cell: (e) => <Mono>{e.type}</Mono> },
        { header: "Object", className: "hidden md:table-cell", cell: (e) => (e.object_id ? <Mono>{e.object_id}</Mono> : "—") },
        { header: "When", cell: (e) => <Time iso={new Date(e.created * 1000).toISOString()} relative /> },
      ]}
    />
  )
}
