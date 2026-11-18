"use client"

import { useRouter, useSearchParams } from "next/navigation"
import { Suspense, useState } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Copyable, Mono, StatusBadge } from "@/components/filters"
import { MachinesOf } from "@/components/machines-of"
import { Facts, Page, Panel } from "@/components/page"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"
import Link from "next/link"

type Lookup = Data<"/v1/admin/support/{reference}">
type Account = Data<"/v1/admin/accounts/{account_id}">
const pattern = /^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

export default function SupportPage() {
  return (
    <Page title="Lookup">
      <Suspense>
        <Lookup />
      </Suspense>
    </Page>
  )
}

function Lookup() {
  const router = useRouter()
  const params = useSearchParams()
  const reference = params.get("reference") ?? ""
  const [text, setText] = useState(reference)
  const valid = pattern.test(reference)
  const query = valid ? `/v1/admin/support/${reference}${params.get("owner_id") !== null ? `?owner_id=${encodeURIComponent(params.get("owner_id") ?? "")}` : ""}` : null
  const lookup = useApiQuery<Lookup>(["support", reference, params.get("owner_id")], query)
  return (
    <>
      <form
        className="flex flex-wrap items-center gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          router.push(`/support?reference=${encodeURIComponent(text.trim())}`)
        }}
      >
        <Input aria-label="Support reference" className="h-8 w-full max-w-md border-transparent bg-surface font-mono md:text-sm dark:bg-surface" placeholder="support_…" value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} />
        <Button type="submit" size="lg">Look up</Button>
      </form>
      {reference && !valid && <p role="alert" className="text-sm text-destructive" data-lookup-invalid>Not a support reference. It starts with support_ and a UUID.</p>}
      {valid && lookup.isError && <ErrorBlock what="this reference" error={lookup.error} retry={() => lookup.refetch()} />}
      {valid && lookup.isPending && <Skeleton className="h-40 w-full" />}
      {lookup.data && <Result data={lookup.data} />}
    </>
  )
}

function Result({ data }: { data: Lookup }) {
  const owners = Object.entries(data.records_by_owner)
  const links = Object.entries(data.sentry_links)
  return (
    <div className="flex flex-col gap-6" data-lookup-result>
      <Facts
        rows={[
          ["Reference", <Copyable key="r" value={data.support_reference} />],
          ["Status", <StatusBadge key="s" state={data.status} />],
          ["Note", data.message],
          ["Dropped records", data.dropped_records],
          ["Sentry", data.sentry_search_url ? <a key="u" className="underline-offset-2 hover:underline" href={data.sentry_search_url} target="_blank" rel="noopener noreferrer">Open in Sentry</a> : "—"],
        ]}
      />
      {links.length > 0 && (
        <Panel title="Sentry events">
          <ul className="text-sm">
            {links.map(([event, url]) => (
              <li key={event} className="border-b border-divider py-2.5">
                <a className="underline-offset-2 hover:underline" href={url} target="_blank" rel="noopener noreferrer"><Mono>{event}</Mono></a>
              </li>
            ))}
          </ul>
        </Panel>
      )}
      {owners.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No records. They expire after 7 days.</p>}
      {owners.map(([owner, records]) => (
        <Panel key={owner || "unattributed"} title={owner ? `Owner ${owner}` : "No owner"}>
          {records.map((r) => (
            <Facts
              key={r.id}
              rows={[
                ["When", <Time key="t" iso={r.at} />],
                [
                  "Occurrences",
                  r.group_occurrences ? (
                    <span key="g" data-failure-group>
                      {r.group_occurrences} occurrence{r.group_occurrences === 1 ? "" : "s"} · first <Time iso={r.group_first_seen as string} /> · latest <Time iso={r.group_last_seen as string} relative />
                    </span>
                  ) : (
                    "—"
                  ),
                ],
                ["Operation", `${r.component} · ${r.operation} · ${r.stage}`],
                ["Outcome", `${r.outcome} · ${r.severity} · ${r.code} · ${r.cause}`],
                ["Release", r.release],
                ["HTTP", r.http_status ?? "—"],
                ["Request", r.request_id ? <Mono key="q">{r.request_id}</Mono> : "—"],
                ["Sentry event", r.sentry_event_id ? <Mono key="e">{r.sentry_event_id}</Mono> : `Not sent (${r.reporting})`],
              ]}
            />
          ))}
          {owner && <Owner id={owner} />}
        </Panel>
      ))}
      {data.truncated && <p className="text-sm text-muted-foreground">Showing the first records only.</p>}
    </div>
  )
}

/** The account behind a record and its machines, so support need not search for them. */
function Owner({ id }: { id: string }) {
  const account = useApiQuery<Account>(["account", id], `/v1/admin/accounts/${encodeURIComponent(id)}`)
  if (account.isError) return <ErrorBlock what="this owner" error={account.error} retry={() => account.refetch()} />
  if (!account.data) return <Skeleton className="h-20 w-full" />
  return (
    <div className="flex flex-col gap-3" data-owner-card>
      <p className="text-sm">
        <Link className="font-medium underline-offset-2 hover:underline" href={`/users/${encodeURIComponent(id)}`}>{account.data.primary_email}</Link> · <StatusBadge state={account.data.status} />
      </p>
      <MachinesOf owner={{ account: id }} />
    </div>
  )
}
