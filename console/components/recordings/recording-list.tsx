"use client"

import { RecordIcon, UsersThreeIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { ApiError, useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { formatDuration, recordingPageSize, recordingsQuery, recordingSentences, type Recording } from "@/lib/recordings"
import { teamQuery } from "@/lib/teams"
import { usePageParam } from "@/lib/use-page-param"
import { useTimeFormat } from "@/lib/use-time-format"
import { ListPager } from "@/components/list-pager"
import { useListScope } from "@/components/list-toolbar"
import { LoadError } from "@/components/load-error"
import { useCurrentMachine } from "@/components/machine-shell"
import { StatusDot } from "@/components/machine-status"
import { RecordingAccess } from "@/components/recordings/recording-access"
import { TableRowsSkeleton } from "@/components/skeletons"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"

/** The machine page's Recordings tab. */
export function MachineRecordings() {
  return <RecordingList machine={useCurrentMachine()} />
}

/** The team's recordings, newest first, or one machine's (its tab, or the machine picked in the header). */
export function RecordingList({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const { scope } = useListScope(tabMachine)
  const teamId = workspace?.kind === "team" ? workspace.id : ""
  const pages = usePageParam(`${teamId}|${scope ?? ""}`)
  const query = useApiQuery(recordingsQuery(teamId, scope ?? undefined, pages.page))
  const team = useApiQuery({ ...teamQuery(teamId), enabled: Boolean(teamId) })
  const emails = new Map(team.data?.members.map((member) => [member.account_id, member.email ?? member.account_id]))

  if (workspace && workspace.kind !== "team") return <TeamsOnly />
  if (query.error instanceof ApiError && query.error.code === "team_plan_required") return <TeamsOnly upgrade />
  const rows = query.data?.recordings ?? []
  if (query.isSuccess && query.data.total === 0)
    return (
      <div className="flex flex-col gap-4">
        <RecordingAccess teamId={teamId} />
        <Empty className="py-16">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <RecordIcon />
            </EmptyMedia>
            <EmptyTitle>No recordings yet</EmptyTitle>
            <EmptyDescription>
              Terminals are recorded by the team&apos;s rules, or when their owner starts recording.
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button variant="outline" size="lg" nativeButton={false} render={<Link href="/team?tab=recordings" />}>
              Recording settings
            </Button>
          </EmptyContent>
        </Empty>
      </div>
    )

  return (
    <section className="flex flex-col gap-3">
      <RecordingAccess teamId={teamId} />
      <table className="w-full table-fixed text-sm" data-recordings>
        <colgroup>
          {!scope && <col className="w-[22%]" />}
          <col />
          <col className="hidden w-[20%] sm:table-column" />
          <col className="hidden w-[14%] md:table-column" />
          <col className="hidden w-[12%] md:table-column" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            {!scope && <th>Machine</th>}
            <th>User</th>
            <th className="hidden sm:table-cell">Started</th>
            <th className="hidden md:table-cell">Duration</th>
            <th className="hidden text-right md:table-cell">Size</th>
          </tr>
        </thead>
        {query.isPending ? (
          <tbody>
            <TableRowsSkeleton
              rows={8}
              columns={[
                !scope && { bar: "w-24" },
                { bar: "w-40" },
                { className: "hidden sm:table-cell", bar: "w-28" },
                { className: "hidden md:table-cell", bar: "w-14" },
                { className: "hidden md:table-cell", bar: "w-12" },
              ]}
            />
          </tbody>
        ) : (
          <tbody className="divide-y divide-divider border-b border-divider">
            {rows.map((recording) => (
              <Row key={recording.id} recording={recording} showMachine={!scope} emails={emails} />
            ))}
          </tbody>
        )}
      </table>
      <ListPager page={pages.page} size={recordingPageSize} total={query.data?.total ?? 0} go={pages.go} />
      {query.isError && (
        <LoadError
          what="recordings"
          error={
            query.error instanceof ApiError && recordingSentences[query.error.code]
              ? new ApiError(403, query.error.code, recordingSentences[query.error.code])
              : query.error
          }
          retry={() => query.refetch()}
        />
      )}
    </section>
  )
}

function Row({ recording, showMachine, emails }: { recording: Recording; showMachine: boolean; emails: Map<string, string> }) {
  const { dateTime, now } = useTimeFormat(60_000)
  const live = recording.state === "recording"
  const deleted = recording.state === "deleted"
  const ended = recording.ended_at ? Date.parse(recording.ended_at) : now
  // The person recorded: the one who started it by hand, else everyone attached while it ran.
  const people = recording.started_by ? [recording.started_by] : recording.accounts
  const who = people.map((account) => emails.get(account) ?? account)
  const href = `/recordings/${encodeURIComponent(recording.id)}`
  return (
    <tr className="relative h-14 hover:bg-surface [&>td]:px-3" data-recording-id={recording.id}>
      {showMachine && <td className="truncate">{recording.machine_name}</td>}
      <td className="truncate">
        {deleted ? (
          <span className="text-muted-foreground">{who.join(", ") || "Unknown"}</span>
        ) : (
          <Link href={href} className="after:absolute after:inset-0">
            {who.join(", ") || "Unknown"}
          </Link>
        )}
        {live && (
          <span className="ml-2 inline-flex items-center gap-1.5 text-muted-foreground">
            <StatusDot online />
            Recording
          </span>
        )}
        {deleted && <span className="ml-2 text-muted-foreground">Deleted from the machine</span>}
      </td>
      <td className="hidden text-muted-foreground sm:table-cell">{dateTime(recording.started_at)}</td>
      <td className="hidden text-muted-foreground md:table-cell">
        {ended ? formatDuration((ended - Date.parse(recording.started_at)) / 1000) : null}
      </td>
      <td className="hidden text-right text-muted-foreground tabular-nums md:table-cell">
        {formatBytes(recording.bytes)}
      </td>
    </tr>
  )
}

/** Personal workspaces and teams without the Teams plan. */
function TeamsOnly({ upgrade }: { upgrade?: boolean }) {
  return (
    <Empty className="py-16" data-recordings-upgrade>
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <UsersThreeIcon />
        </EmptyMedia>
        <EmptyTitle>Session recordings are part of Teams</EmptyTitle>
        <EmptyDescription>
          {upgrade
            ? "Upgrade this team to record terminal sessions and replay them here."
            : "Record your team's terminal sessions and replay them here."}
        </EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button size="lg" nativeButton={false} render={<Link href={upgrade ? "/billing" : "/team?create=1"} />}>
          {upgrade ? "Upgrade" : "Create team"}
        </Button>
      </EmptyContent>
    </Empty>
  )
}
