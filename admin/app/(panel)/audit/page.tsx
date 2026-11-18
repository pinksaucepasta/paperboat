"use client"

import { useMemo, useState } from "react"
import { DataTable } from "@/components/data-table"
import { Filters, Mono, SearchBox, StatusBadge } from "@/components/filters"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import { Input } from "@/components/ui/input"
import type { Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

/** An <input type=datetime-local> value to RFC 3339 UTC, or "" when empty or unparsable. */
function toRfc3339(local: string): string {
  const parsed = Date.parse(local)
  return Number.isNaN(parsed) ? "" : new Date(parsed).toISOString()
}

export default function AuditPage() {
  const [actor, setActor] = useState("")
  const [eventType, setEventType] = useState("")
  const [resourceType, setResourceType] = useState("")
  const [resourceId, setResourceId] = useState("")
  const [outcome, setOutcome] = useState("")
  const [since, setSince] = useState("")
  const [until, setUntil] = useState("")
  const filters = useMemo(
    () => ({ actor, event_type: eventType, resource_type: resourceType, resource_id: resourceId, outcome, since: toRfc3339(since), until: toRfc3339(until) }),
    [actor, eventType, resourceType, resourceId, outcome, since, until],
  )
  const list = useCursorList<Data<"/v1/admin/audit">>("/v1/admin/audit", filters)
  return (
    <Page title="Audit log">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Actor id" placeholder="Actor id" value={actor} onChange={setActor} />
        <SearchBox label="Event type" placeholder="Event type" value={eventType} onChange={setEventType} />
        <SearchBox label="Resource type" placeholder="Resource type" value={resourceType} onChange={setResourceType} />
        <SearchBox label="Resource id" placeholder="Resource id" value={resourceId} onChange={setResourceId} />
        <Filters items={[{ label: "Outcome", value: outcome, onChange: setOutcome, options: [["changed", "Changed"], ["unchanged", "Unchanged"], ["uncertain", "Uncertain"]] }]} />
        <Input aria-label="From date" type="datetime-local" className="w-48" value={since} onChange={(e) => setSince(e.target.value)} />
        <Input aria-label="Until date" type="datetime-local" className="w-48" value={until} onChange={(e) => setUntil(e.target.value)} />
      </div>
      <DataTable
        what="audit events"
        list={list}
        rows={(d) => d.events}
        rowKey={(e) => e.id}
        empty="No events match."
        columns={[
          { header: "When", cell: (e) => <Time iso={e.created_at} /> },
          { header: "Actor", className: "hidden md:table-cell", cell: (e) => (e.actor_user_id ? <Mono>{e.actor_user_id}</Mono> : e.actor_type) },
          { header: "Event", cell: (e) => <Mono>{e.event_type}</Mono> },
          { header: "Resource", className: "hidden md:table-cell", cell: (e) => <Mono>{`${e.resource_type}${e.resource_id ? `: ${e.resource_id}` : ""}`}</Mono> },
          { header: "Outcome", cell: (e) => (e.outcome ? <StatusBadge state={e.outcome} /> : "—") },
          { header: "Request", className: "hidden lg:table-cell", cell: (e) => (e.request_id ? <Mono>{e.request_id}</Mono> : "—") },
        ]}
      />
    </Page>
  )
}
