"use client"

import { useRouter, useSearchParams } from "next/navigation"
import { Suspense, useState } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
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
    <Page title="Support lookup" description="One page for a support reference: the stored fault, its Sentry links, and the owner's account and machines.">
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
        <Input aria-label="Support reference" className="w-96 max-w-full font-mono" placeholder="support_xxxxxxxx-xxxx-4xxx-xxxx-xxxxxxxxxxxx" value={text} onChange={(e) => setText(e.target.value)} />
        <Button type="submit">Look up</Button>
      </form>
      {reference && !valid && <p role="alert" className="text-sm text-destructive" data-lookup-invalid>That is not a support reference. They look like support_ followed by a UUID.</p>}
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
          ["Reference", <Mono key="r">{data.support_reference}</Mono>],
          ["Status", <StatusBadge key="s" state={data.status} />],
          ["Note", data.message],
          ["Records dropped by this server", data.dropped_records],
          ["Sentry", data.sentry_search_url ? <a key="u" className="underline" href={data.sentry_search_url} target="_blank" rel="noopener noreferrer">Search this reference in Sentry</a> : "—"],
        ]}
      />
      {links.length > 0 && (
        <Panel title="Sentry events">
          <ul className="border-t border-divider text-sm">
            {links.map(([event, url]) => (
              <li key={event} className="border-b border-divider py-2">
                <a className="underline" href={url} target="_blank" rel="noopener noreferrer"><Mono>{event}</Mono></a>
              </li>
            ))}
          </ul>
        </Panel>
      )}
      {owners.length === 0 && <p className="text-sm text-muted-foreground">No stored record carries this reference (records expire after seven days).</p>}
      {owners.map(([owner, records]) => (
        <Panel key={owner || "unattributed"} title={owner ? `Owner ${owner}` : "Not attributed to an account"}>
          {records.map((r) => (
            <Facts
              key={r.id}
              rows={[
                ["When", <Time key="t" iso={r.at} />],
                [
                  "This failure",
                  r.group_occurrences ? (
                    <span key="g" data-failure-group>
                      {r.group_occurrences} occurrence{r.group_occurrences === 1 ? "" : "s"}, first <Time iso={r.group_first_seen as string} />, latest <Time iso={r.group_last_seen as string} relative />
                    </span>
                  ) : (
                    "—"
                  ),
                ],
                ["Operation", `${r.component} · ${r.operation} · ${r.stage}`],
                ["Outcome", `${r.outcome} (${r.severity}), code ${r.code}, cause ${r.cause}`],
                ["Release", r.release],
                ["HTTP status", r.http_status ?? "—"],
                ["Request id", r.request_id ? <Mono key="q">{r.request_id}</Mono> : "—"],
                ["Sentry event", r.sentry_event_id ? <Mono key="e">{r.sentry_event_id}</Mono> : `not sent (${r.reporting})`],
              ]}
            />
          ))}
          {owner && <Owner id={owner} />}
        </Panel>
      ))}
      {data.truncated && <p className="text-sm text-muted-foreground">More records exist than shown; narrow by owner.</p>}
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
        <Link className="underline" href={`/users/${encodeURIComponent(id)}`}>{account.data.primary_email}</Link> · <StatusBadge state={account.data.status} />
      </p>
      <MachinesOf owner={{ account: id }} />
    </div>
  )
}
