"use client"

import { useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Facts, Page, Panel } from "@/components/page"
import { StatusIncidents, StatusMaintenance, type Snapshot } from "@/components/status-forms"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery } from "@/lib/api"

type State = "operational" | "degraded" | "major" | "delayed" | "maintenance"
const word: Record<string, string> = { operational: "Operational", degraded: "Degraded", major: "Major outage", delayed: "Status data delayed", maintenance: "Maintenance" }
const dot = (s: string) => (s === "operational" ? "bg-success" : s === "major" ? "bg-destructive" : s === "degraded" || s === "maintenance" ? "bg-amber-500" : "bg-muted-foreground/35")
const view: Record<string, string> = { pass: "passing", fail: "failing", slow: "slow", mixed: "mixed", stale: "no recent data" }

function StateWord({ state }: { state: State | string }) {
  return (
    <span className="inline-flex items-center gap-2 whitespace-nowrap" data-status-state={state}>
      <span className={`size-1.5 shrink-0 rounded-full ${dot(state)}`} aria-hidden />
      {word[state] ?? state}
    </span>
  )
}

/** The public status page's state, the evidence behind each unit, probe health, incidents and maintenance. */
export default function StatusPage() {
  const query = useApiQuery<Snapshot & { configured: boolean }>(["status"], "/status/state", 30_000)
  const client = useQueryClient()
  const refresh = () => client.invalidateQueries({ queryKey: ["status"] })
  const s = query.data
  return (
    <Page title="Status page">
      {query.isError ? (
        <ErrorBlock what="the status page" error={query.error} retry={() => query.refetch()} />
      ) : !s ? (
        <Skeleton className="h-48 w-full" />
      ) : !s.configured ? (
        <p className="text-sm text-muted-foreground" data-status-not-configured>
          The status page is not set up for this panel. Set STATUS_API_URL and STATUS_ADMIN_TOKEN to connect it.
        </p>
      ) : (
        <Loaded s={s} refresh={refresh} />
      )}
    </Page>
  )
}

function Loaded({ s, refresh }: { s: Snapshot; refresh: () => void }) {
  const [open, setOpen] = useState<string | null>(null)
  const iso = (ms: number) => new Date(ms).toISOString()
  return (
    <>
      <Panel title="Components">
        <div className="divide-y divide-divider border-y border-divider">
          {s.public.components.map((c) => (
            <div key={c.id} className="py-2 text-sm" data-status-component={c.id}>
              <button type="button" className="flex w-full items-center justify-between gap-3 py-1 text-left" onClick={() => setOpen(open === c.id ? null : c.id)} aria-expanded={open === c.id}>
                <span className="font-medium">{c.name}</span>
                <StateWord state={c.state} />
              </button>
              {open === c.id && (
                <ul className="mb-2 flex flex-col gap-1 pl-3 text-xs text-muted-foreground">
                  {c.units.map((u) => (
                    <li key={u.id} className="flex flex-wrap items-center gap-x-3" data-status-unit={u.id}>
                      <span className="w-40 shrink-0 text-foreground">{u.name}</span>
                      <StateWord state={u.state} />
                      <span>
                        {(s.admin.evidence[u.id] ?? []).map((e) => `${e.vantage} ${view[e.view] ?? e.view}${e.last ? ` (${e.last.ms} ms)` : ""}`).join(" · ")}
                      </span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </div>
      </Panel>
      <Panel title="Probe hosts">
        <div className="divide-y divide-divider border-y border-divider text-sm">
          {s.admin.vantages.map((v) => (
            <div key={v.vantage} className="flex flex-wrap items-center justify-between gap-2 py-2" data-status-vantage={v.vantage}>
              <span className="font-medium">{v.vantage}</span>
              <span className="text-muted-foreground">
                {v.last === null ? "Never reported" : <>Last report <Time iso={iso(v.last)} relative />{v.net_ok === false ? " · its internet is down, results discarded" : ""}</>}
                {v.build ? ` · ${v.build}` : ""}
              </span>
            </div>
          ))}
        </div>
      </Panel>
      <StatusIncidents s={s} refresh={refresh} />
      <StatusMaintenance s={s} refresh={refresh} />
      <Panel title="Email subscribers">
        <Facts rows={[["Confirmed", s.admin.subscribers]]} />
      </Panel>
    </>
  )
}
