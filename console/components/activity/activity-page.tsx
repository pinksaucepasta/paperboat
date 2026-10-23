"use client"

import { ClockCounterClockwiseIcon, DownloadSimpleIcon } from "@phosphor-icons/react"
import { useInfiniteQuery } from "@tanstack/react-query"
import Link from "next/link"
import { Fragment, useDeferredValue, useState } from "react"
import { meQuery, useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { teamQuery } from "@/lib/teams"
import {
  activityAreas,
  activityExportHref,
  activityLive,
  activityPeriods,
  activityQuery,
  actorName,
  describe,
  periodStart,
  targetHref,
  viaToken,
  type ActivityArea,
  type ActivityFilter,
  type ActivityPeriod,
} from "@/lib/activity"
import { AuditStreams } from "@/components/activity/audit-streams"
import { SearchField, useListScope, ViewToggle } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { Choice } from "@/components/tunnels/share-file"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { useTimeFormat } from "@/lib/use-time-format"

/** The machine page's Activity tab. */
export function MachineActivity() {
  return <ActivityPage machine={useCurrentMachine()} />
}

/** "Today", "Yesterday" or the weekday and date; the stable date until hydrated (`now` 0). */
function dayLabel(iso: string, now: number, date: (iso: string, style: "weekday") => string) {
  if (!now) return date(iso, "weekday")
  const day = new Date(iso)
  const today = new Date(now)
  today.setHours(0, 0, 0, 0)
  const start = new Date(day)
  start.setHours(0, 0, 0, 0)
  const diff = Math.round((today.getTime() - start.getTime()) / 86_400_000)
  return diff === 0 ? "Today" : diff === 1 ? "Yesterday" : date(iso, "weekday")
}

/**
 * What happened in the workspace, newest first, grouped by day: who did what to which machine,
 * terminal, tunnel or file. Search and a kind filter narrow it; a machine's tab shows only its own.
 */
export function ActivityPage({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const time = useTimeFormat(60_000)
  const { scope } = useListScope(tabMachine)
  const { data: me } = useApiQuery(meQuery)
  const { data: team } = useApiQuery({ ...teamQuery(workspace?.id ?? ""), enabled: workspace?.kind === "team" })
  const memberNames = new Map(team?.members?.map((member) => [member.account_id, member.display_name || member.email]))
  const [area, setArea] = useState<ActivityArea>("all")
  const [search, setSearch] = useState("")
  const [denied, setDenied] = useState(false)
  const [period, setPeriod] = useState<{ name: ActivityPeriod; since?: string }>({ name: "all" })
  const q = useDeferredValue(search.trim())
  const filter: ActivityFilter = { workspace: workspace?.id ?? "", machineId: scope, area, q, denied, since: period.since }
  const activity = useInfiniteQuery({ ...activityQuery(filter), enabled: Boolean(workspace) })
  // The audit log is exported by the owner, admins and auditors of a team (teams-business.md).
  const canExport =
    activityLive.export && workspace?.kind === "team" && ["owner", "admin", "auditor"].includes(workspace.role ?? "")
  const filtered = q || area !== "all" || denied || period.name !== "all"
  // Streaming: owners and admins configure it, auditors see it (teams-business.md).
  const role = workspace?.kind === "team" ? (workspace.role ?? "") : ""
  const streaming = activityLive.streaming && !tabMachine && ["owner", "admin", "auditor"].includes(role)
  const [view, setView] = useState<"events" | "streaming">("events")
  if (streaming && view === "streaming")
    return (
      <section className="flex flex-col gap-6">
        <ViewToggle value={view} onChange={setView} views={[{ value: "events", label: "Events" }, { value: "streaming", label: "Streaming" }]} />
        <AuditStreams team={workspace!.id} canConfigure={role !== "auditor"} />
      </section>
    )
  const events = activity.data?.pages.flatMap((page) => page.items) ?? []

  const groups: { label: string; events: typeof events }[] = []
  for (const event of events) {
    const label = dayLabel(event.occurred_at, time.now, time.date)
    const group = groups.at(-1)
    if (group?.label === label) group.events.push(event)
    else groups.push({ label, events: [event] })
  }

  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        {streaming && (
          <ViewToggle value={view} onChange={setView} views={[{ value: "events", label: "Events" }, { value: "streaming", label: "Streaming" }]} />
        )}
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
        {activityLive.filters && (
          <>
            <Choice
              label={activityPeriods[period.name]}
              value={period.name}
              options={Object.entries(activityPeriods).map(([value, label]) => ({ value, label }))}
              onChange={(value) =>
                setPeriod({ name: value as ActivityPeriod, since: periodStart(value as ActivityPeriod, time.now) })
              }
            />
            <Choice
              label={denied ? "Only denied" : "All results"}
              value={denied ? "denied" : "all"}
              options={[
                { value: "all", label: "All results" },
                { value: "denied", label: "Only denied" },
              ]}
              onChange={(value) => setDenied(value === "denied")}
            />
          </>
        )}
        {canExport && (
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" className="ml-auto" />}>
              <DownloadSimpleIcon data-icon="inline-start" />
              Export
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem render={<a href={activityExportHref(filter, "csv")} download />}>CSV (spreadsheets)</DropdownMenuItem>
              <DropdownMenuItem render={<a href={activityExportHref(filter, "json")} download />}>JSON</DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
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
            <EmptyTitle>{filtered ? "Nothing matches" : "No activity yet"}</EmptyTitle>
            <EmptyDescription>
              {filtered
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
                        <time dateTime={event.occurred_at} title={time.dateTime(event.occurred_at)}>
                          {time.date(event.occurred_at, "time")}
                        </time>
                      </span>
                      <span className="flex size-6 shrink-0 items-center justify-center rounded-md bg-primary/12 text-xs font-medium text-primary">
                        {actor.charAt(0).toUpperCase()}
                      </span>
                      <p className="min-w-0 flex-1 truncate">
                        <span className="font-medium">{actor}</span>
                        {viaToken(event) && <span className="text-muted-foreground"> with an API token</span>}{" "}
                        {event.result === "denied" && !describe(event).startsWith("was refused")
                          ? `was refused: ${describe(event)}`
                          : describe(event)}
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
                      {event.result === "denied" && (
                        <Badge variant="destructive" className="shrink-0 font-normal" title={event.reason}>
                          Denied
                        </Badge>
                      )}
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
