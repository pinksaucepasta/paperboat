"use client"

import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { Time } from "@/components/time"
import { LoadError } from "@/components/load-error"
import { Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  eventLabel,
  historyPageSize,
  historyQuery,
  type HistoryEvent,
  type HistoryLog,
  type HistoryPart,
} from "@/lib/tunnels"
import { cn } from "@/lib/utils"

const levelColor = { debug: "bg-muted-foreground/40", info: "bg-muted-foreground", warn: "bg-amber-500", error: "bg-destructive" }

/** A tunnel's or preview's events and logs, newest page last, paged forward with the server's cursors. */
export function ResourceHistory({ kind, id }: { kind: "tunnel" | "preview"; id: string }) {
  const [part, setPart] = useState<HistoryPart>("events")
  // The cursor that starts each page seen so far; the first page has none.
  const [cursors, setCursors] = useState([""])
  const page = cursors.length
  const query = useQuery(historyQuery(kind, id, part, cursors[page - 1]))
  const next = query.data?.next_cursor
  const items = query.data?.items ?? []
  const choose = (value: HistoryPart) => {
    setPart(value)
    setCursors([""])
  }
  return (
    <Section
      title="History"
      action={
        <ToggleGroup
          value={[part]}
          onValueChange={(value) => value[0] && choose(value[0] as HistoryPart)}
          variant="outline"
          spacing={0}
        >
          <ToggleGroupItem value="events">Events</ToggleGroupItem>
          <ToggleGroupItem value="logs">Logs</ToggleGroupItem>
        </ToggleGroup>
      }
    >
      {query.isError ? (
        <LoadError what={part} error={query.error} retry={() => query.refetch()} />
      ) : !query.data ? (
        Array.from({ length: 3 }, (_, index) => (
          <div key={index} className="flex min-h-12 items-center gap-4 border-b border-divider">
            <Skeleton className="h-4 w-36" />
            <Skeleton className="h-4 w-48 max-w-full" />
          </div>
        ))
      ) : items.length === 0 ? (
        <p className="border-b border-divider py-3 text-center text-sm text-muted-foreground">
          {page > 1 ? "Nothing more." : `No ${part} yet.`}
        </p>
      ) : (
        items.map((item) => (
          <div key={item.id} className="flex min-h-12 items-center gap-4 border-b border-divider text-sm">
            <span className="w-40 shrink-0 text-muted-foreground">
              <Time iso={item.occurred_at} />
            </span>
            {part === "logs" ? <LogRow log={item as HistoryLog} /> : <EventRow event={item as HistoryEvent} />}
          </div>
        ))
      )}
      {(page > 1 || next) && (
        <nav className="flex items-center justify-end gap-3 py-3 text-sm text-muted-foreground" aria-label="Pages">
          <span className="tabular-nums">
            {(page - 1) * historyPageSize + 1}–{(page - 1) * historyPageSize + items.length}
          </span>
          <Button variant="outline" size="sm" disabled={page === 1} onClick={() => setCursors(cursors.slice(0, -1))}>
            Previous
          </Button>
          <Button variant="outline" size="sm" disabled={!next} onClick={() => next && setCursors([...cursors, next])}>
            Next
          </Button>
        </nav>
      )}
    </Section>
  )
}

function EventRow({ event }: { event: HistoryEvent }) {
  return <span className="min-w-0 truncate">{eventLabel(event.event_type)}</span>
}

function LogRow({ log }: { log: HistoryLog }) {
  return (
    <span className="flex min-w-0 items-center gap-2">
      <span className={cn("size-1.5 shrink-0 rounded-full", levelColor[log.level])} title={log.level} />
      <span className="min-w-0 truncate font-mono text-code">{log.message}</span>
    </span>
  )
}
