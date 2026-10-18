"use client"

import { ClockCounterClockwiseIcon } from "@phosphor-icons/react"
import { useInfiniteQuery } from "@tanstack/react-query"
import Link from "next/link"
import { Fragment, useDeferredValue, useState } from "react"
import { meQuery, useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { teamQuery } from "@/lib/teams"
import { activityAreas, activityQuery, actorName, describe, targetHref, type ActivityArea } from "@/lib/activity"
import { SearchField, useListScope } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { Choice } from "@/components/tunnels/share-file"
import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

/** The machine page's Activity tab. */
export function MachineActivity() {
  return <ActivityPage machine={useCurrentMachine()} />
}

const dayFormat = new Intl.DateTimeFormat(undefined, { weekday: "long", month: "long", day: "numeric" })
const timeFormat = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" })

function dayLabel(iso: string) {
  const day = new Date(iso)
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  const start = new Date(day)
  start.setHours(0, 0, 0, 0)
  const diff = Math.round((today.getTime() - start.getTime()) / 86_400_000)
  return diff === 0 ? "Today" : diff === 1 ? "Yesterday" : dayFormat.format(day)
}

/**
 * What happened in the workspace, newest first, grouped by day: who did what to which machine,
 * terminal, tunnel or file. Search and a kind filter narrow it; a machine's tab shows only its own.
 */
export function ActivityPage({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const { scope } = useListScope(tabMachine)
  const { data: me } = useApiQuery(meQuery)
  const { data: team } = useApiQuery({ ...teamQuery(workspace?.id ?? ""), enabled: workspace?.kind === "team" })
  const memberNames = new Map(team?.members?.map((member) => [member.account_id, member.display_name || member.email]))
  const [area, setArea] = useState<ActivityArea>("all")
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const activity = useInfiniteQuery({
    ...activityQuery({ workspace: workspace?.id ?? "", machineId: scope, area, q }),
    enabled: Boolean(workspace),
  })
  const events = activity.data?.pages.flatMap((page) => page.items) ?? []

  const groups: { label: string; events: typeof events }[] = []
  for (const event of events) {
    const label = dayLabel(event.occurred_at)
    const group = groups.at(-1)
    if (group?.label === label) group.events.push(event)
    else groups.push({ label, events: [event] })
  }

  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search activity" />
        <span className="flex items-center gap-1.5 text-sm text-muted-foreground">
          Show
          <Choice
            label={activityAreas[area]}
            value={area}
            options={Object.entries(activityAreas).map(([value, label]) => ({ value, label }))}
            onChange={(value) => setArea(value as ActivityArea)}
          />
        </span>
      </div>

      {activity.isPending ? (
        <div className="flex flex-col gap-3 pt-3">
          {Array.from({ length: 6 }, (_, index) => (
            <Skeleton key={index} className="h-5 w-2/3" />
          ))}
        </div>
      ) : activity.isError ? (
        <p className="py-12 text-center text-sm text-muted-foreground">
          Couldn’t load activity. {activity.error.message}
        </p>
      ) : !events.length ? (
        <Empty className="py-16">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <ClockCounterClockwiseIcon />
            </EmptyMedia>
            <EmptyTitle>{q || area !== "all" ? "Nothing matches" : "No activity yet"}</EmptyTitle>
            <EmptyDescription>
              {q || area !== "all"
                ? "Try another search or show all activity."
                : "Machines added, terminals opened, tunnels created and other changes show up here."}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <div className="flex flex-col">
          {groups.map((group) => (
            <Fragment key={group.label}>
              <h2 className="pt-5 pb-2 text-xs font-medium">{group.label}</h2>
              <ol className="flex flex-col divide-y divide-divider border-y border-divider">
                {group.events.map((event) => {
                  const href = targetHref(event)
                  const actor = actorName(event, me?.id, memberNames)
                  return (
                    <li key={event.id} className="flex min-h-12 items-center gap-4 py-2 text-sm">
                      <span className="w-16 shrink-0 text-xs text-muted-foreground tabular-nums">
                        <time dateTime={event.occurred_at} title={new Date(event.occurred_at).toLocaleString()}>
                          {timeFormat.format(new Date(event.occurred_at))}
                        </time>
                      </span>
                      <span className="flex size-6 shrink-0 items-center justify-center rounded-md bg-primary/12 text-xs font-medium text-primary">
                        {actor.charAt(0).toUpperCase()}
                      </span>
                      <p className="min-w-0 flex-1 truncate">
                        <span className="font-medium">{actor}</span> {describe(event)}
                        {event.target?.name && (
                          <>
                            {" "}
                            {href ? (
                              <Link href={href} className="font-medium underline-offset-4 hover:underline">
                                {event.target.name}
                              </Link>
                            ) : (
                              <span className="font-medium">{event.target.name}</span>
                            )}
                          </>
                        )}
                        {!scope && event.machine && event.target?.kind !== "machine" && (
                          <span className="text-muted-foreground">
                            {" "}
                            on{" "}
                            <Link
                              href={`/machines/${encodeURIComponent(event.machine.id)}/activity`}
                              className="underline-offset-4 hover:text-foreground hover:underline"
                            >
                              {event.machine.name}
                            </Link>
                          </span>
                        )}
                      </p>
                      {event.source && (
                        <span className="hidden shrink-0 text-xs text-muted-foreground sm:inline">
                          {event.source === "cli"
                            ? "CLI"
                            : event.source === "api"
                              ? "API"
                              : event.source === "machine"
                                ? "Machine"
                                : "Console"}
                        </span>
                      )}
                    </li>
                  )
                })}
              </ol>
            </Fragment>
          ))}
          {activity.hasNextPage && (
            <Button
              variant="secondary"
              className="mt-4 self-center"
              onClick={() => activity.fetchNextPage()}
              disabled={activity.isFetchingNextPage}
            >
              {activity.isFetchingNextPage ? "Loading…" : "Load more"}
            </Button>
          )}
        </div>
      )}
    </section>
  )
}
