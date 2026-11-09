"use client"

import { ArrowSquareOutIcon, PowerIcon, TerminalIcon, TerminalWindowIcon } from "@phosphor-icons/react"
import { useQueryClient } from "@tanstack/react-query"
import { Fragment, useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { useApiQuery, useMachines, type Machine } from "@/lib/api"
import { formatRelative } from "@/lib/format"
import { closeTerminal, terminalCountsQuery, terminalKeys, terminalPageSize, terminalsPageQuery } from "@/lib/terminal/queries"
import { usePageParam } from "@/lib/use-page-param"
import { ListPager } from "@/components/list-pager"
import { terminalCanAttach, type TerminalSession } from "@/lib/terminal/use-terminal-connection"
import { useCurrentMachine } from "@/components/machine-shell"
import { NewTerminalButton } from "@/components/terminal/new-terminal"
import { StatusDot } from "@/components/machine-status"
import { ConfirmAction, SearchField, useListScope, ViewToggle } from "@/components/list-toolbar"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Badge } from "@/components/ui/badge"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { LoadError } from "@/components/load-error"
import { TableRowsSkeleton } from "@/components/skeletons"
import { Skeleton } from "@/components/ui/skeleton"
import { copyWithToast } from "@/lib/clipboard"
import { useTimeFormat } from "@/lib/use-time-format"
import { useLastCount } from "@/lib/last-count"
import { cn } from "@/lib/utils"

const day = 86_400_000

/**
 * When a terminal was last in use: open ones are current (the server's list carries no
 * participants or reliable activity time for them); closed ones ended at `closed_at`, or, without
 * one, were last active at `last_active_at`.
 */
function lastUsed(session: TerminalSession, now: number): number {
  if (terminalCanAttach(session)) return now
  const ended = session.closed_at ?? session.last_active_at
  return ended ? Date.parse(ended) : 0
}

/** Google Photos-style groups: Today, Yesterday, earlier this week, earlier this month, then months. */
function groupOf(
  time: number,
  now: number,
  date: (iso: string, style: "month") => string,
): { key: string; label: string } {
  if (!time) return { key: "older", label: "Earlier" }
  const today = new Date(now)
  today.setHours(0, 0, 0, 0)
  const start = today.getTime()
  const when = new Date(time)
  if (time >= start) return { key: "today", label: "Today" }
  if (time >= start - day) return { key: "yesterday", label: "Yesterday" }
  if (time >= start - 6 * day) return { key: "week", label: "Earlier this week" }
  if (when.getMonth() === today.getMonth() && when.getFullYear() === today.getFullYear())
    return { key: "month", label: "Earlier this month" }
  return { key: `${when.getFullYear()}-${when.getMonth()}`, label: date(when.toISOString(), "month") }
}

type View = "open" | "closed"

const unreachableNote =
  "Their machine isn’t reporting them, usually because it’s offline. They come back when it reconnects, and close on their own after 30 days without it."

/** The machine page's Terminals tab. */
export function MachineTerminals() {
  return <TerminalList machine={useCurrentMachine()} />
}

/**
 * Every terminal you own or that's shared with you, or only those on one machine (its Terminals
 * tab, or the machine picked in the header), where the Machine column would only repeat it.
 */
export function TerminalList({ machine: tabMachine }: { machine?: Machine }) {
  const { scope, machine } = useListScope(tabMachine)
  const { now, date } = useTimeFormat(60_000)
  const [view, setView] = useState<View>("open")
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(`${view}|${q}|${scope ?? ""}`)
  const counts = useApiQuery(terminalCountsQuery(scope ?? undefined))
  const query = useApiQuery(terminalsPageQuery({ closed: view === "closed", q, page: pages.page, machine: scope ?? undefined }))
  const total = query.data?.total ?? 0
  const shown = (query.data?.items ?? []).map((session) => ({ session, time: lastUsed(session, now) }))

  // Open terminals are current: live ones first, then those whose machine isn't reporting them (shown
  // as last reported). Closed ones group by when they were used.
  const groups: { key: string; label: string; note?: string; rows: typeof shown }[] = []
  if (view === "open") {
    const live = shown.filter(({ session }) => session.state !== "unavailable")
    const away = shown.filter(({ session }) => session.state === "unavailable")
    if (live.length) groups.push({ key: "open", label: "", rows: live })
    if (away.length)
      groups.push({
        key: "away",
        label: live.length ? "Not reachable" : "",
        note: unreachableNote,
        rows: away,
      })
  } else
    // The server lists closed terminals newest closed first, which is the order of the groups.
    for (const row of shown) {
      const { key, label } = groupOf(row.time, now, date)
      const group = groups.at(-1)
      if (group?.key === key) group.rows.push(row)
      else groups.push({ key, label, rows: [row] })
    }

  // Terminals are yours across workspaces, so the count isn't per workspace.
  const remembered = `terminals:${scope ?? ""}:${view}`
  const lastRows = useLastCount(remembered, query.data ? shown.length : undefined)
  // Whether the list opened with the unreachable note, so the loading state reserves its line.
  const lastNote = useLastCount(`${remembered}:note`, query.data ? (groups[0]?.note ? 1 : 0) : undefined)
  // Running shows only when a visible terminal reports what it runs (unreachable ones never do), so
  // the name gets the room otherwise. The first page is read on the server (terminals/page.tsx);
  // before any data (the static shell) the last visit's answer decides, hidden on a first visit.
  const reporting = shown.some(
    ({ session }) => session.state !== "unavailable" && Boolean(session.title || session.foreground_process),
  )
  const lastRunning = useLastCount(`${remembered}:running`, query.data && view === "open" ? Number(reporting) : undefined)
  const running = view === "open" && (query.data ? reporting : lastRunning === 1)

  if (counts.data && counts.data.open + counts.data.closed === 0) {
    return (
      <Empty className="py-16">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <TerminalIcon />
          </EmptyMedia>
          <EmptyTitle>No terminals yet</EmptyTitle>
          <EmptyDescription>
            {machine ? `Open a shell on ${machine.alias}.` : "Open a shell on any of your machines."} It keeps running
            when you close the tab, and you can share it live.
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <NewTerminalButton machine={machine} />
        </EmptyContent>
      </Empty>
    )
  }

  return (
    <section className="flex flex-col gap-3">
      {/* On a phone the search takes its own line, so the toggle and buttons keep their size. */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="w-full sm:w-auto sm:max-w-sm sm:flex-1">
          <SearchField value={search} onChange={setSearch} placeholder="Search terminals" />
        </div>
        <ViewToggle
          value={view}
          onChange={setView}
          views={[
            { value: "open", label: "Open", count: counts.data?.open },
            { value: "closed", label: "Closed", count: counts.data?.closed },
          ]}
        />
        <div className="ml-auto">
          <NewTerminalButton machine={machine} />
        </div>
      </div>

      <div>
        {/* Fixed column widths: loading data (or a long name) never moves the columns. */}
        <table className="w-full table-fixed text-sm">
          <colgroup>
            <col />
            {!scope && <col className="hidden w-[16%] md:table-column" />}
            {running && <col className="hidden w-[22%] lg:table-column" />}
            <col className="hidden w-[24%] lg:table-column" />
            <col className="hidden w-[14%] sm:table-column" />
            <col className="w-12" />
          </colgroup>
          <thead className="border-b border-divider text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Terminal</th>
              {!scope && <th className="hidden md:table-cell">Machine</th>}
              {running && <th className="hidden lg:table-cell">Running</th>}
              <th className="hidden lg:table-cell">Folder</th>
              <th className="hidden sm:table-cell">{view === "open" ? "Status" : "Ended"}</th>
              <th className="w-12" aria-label="Actions" />
            </tr>
          </thead>
          {query.isPending ? (
            <tbody>
              {lastNote ? (
                <tr>
                  <SpanShown scope={Boolean(scope)} running={running}>
                    <span className="block text-muted-foreground">{unreachableNote}</span>
                  </SpanShown>
                </tr>
              ) : null}
              <TableRowsSkeleton
                rows={lastRows ?? Math.min((view === "open" ? counts.data?.open : counts.data?.closed) ?? 12, terminalPageSize)}
                columns={[
                  { bar: "w-48" },
                  !scope && { className: "hidden md:table-cell", bar: "w-20" },
                  running && { className: "hidden lg:table-cell", bar: "w-16" },
                  { className: "hidden lg:table-cell", bar: "w-28" },
                  { className: "hidden sm:table-cell", bar: "w-24", second: view === "open" ? "w-16" : undefined },
                  { empty: true },
                ]}
              />
            </tbody>
          ) : (
            groups.map((group) => (
              <Fragment key={group.key}>
                {(group.label || group.note) && (
                  <tbody>
                    <tr>
                      <SpanShown scope={Boolean(scope)} running={running}>
                        {group.label && <span className="block font-medium text-foreground">{group.label}</span>}
                        {group.note && <span className="block text-muted-foreground">{group.note}</span>}
                      </SpanShown>
                    </tr>
                  </tbody>
                )}
                <tbody className="divide-y divide-divider border-b border-divider">
                  {group.rows.map(({ session, time }) => (
                    <Row key={session.id} session={session} time={time} now={now} showMachine={!scope} showRunning={running} />
                  ))}
                </tbody>
              </Fragment>
            ))
          )}
        </table>
        <ListPager page={pages.page} size={terminalPageSize} total={total} go={pages.go} />
        {query.isError && <LoadError what="terminals" error={query.error} retry={() => query.refetch()} />}
        {query.isSuccess && shown.length === 0 && (
          <p className="py-12 text-center text-sm text-muted-foreground">
            {q
              ? `No terminals match “${q}”`
              : view === "open"
                ? "No open terminals"
                : "No closed terminals"}
          </p>
        )}
      </div>
    </section>
  )
}

/** The command that continues this terminal in a terminal app on your computer. */
function localCommand(session: TerminalSession) {
  return session.role === "owner"
    ? `pb session attach ${session.target.name} ${session.id}`
    : `pb session join ${session.id}`
}

/**
 * Whether anyone is in an open terminal right now: attached means a tab or a `pb` client is
 * connected (the server counts participants); detached means it keeps running with nobody in it.
 * Unavailable means the machine isn’t reporting it: offline (`target.status`, with when it was last
 * seen), or online but not answering for it.
 */
function TerminalStatus({ session, now }: { session: TerminalSession; now: number }) {
  const machines = useMachines()
  const { date } = useTimeFormat()
  if (session.state === "attached")
    return (
      <span className="flex items-center gap-2">
        <StatusDot online />
        In use
      </span>
    )
  if (session.state === "unavailable") {
    if (session.target.status !== "offline") return <span className="text-muted-foreground">Machine not answering</span>
    const seen = machines.data?.find((machine) => machine.id === session.target.machine_id)?.last_seen_at
    return (
      <span className="flex flex-col text-muted-foreground">
        <span>Machine offline</span>
        {/* The second line holds its place while the machines list (last seen) loads. */}
        {seen ? (
          <span className="text-xs">since {date(seen, now && now - Date.parse(seen) < day ? "time" : "day")}</span>
        ) : (
          machines.isPending && <Skeleton className="my-[3px] h-2.5 w-16" />
        )}
      </span>
    )
  }
  return (
    <span className="flex items-center gap-2">
      <StatusDot online={false} />
      Idle
    </span>
  )
}

/** Why a closed terminal ended (`closed_reason`, absent on terminals closed before the server kept it). */
function closedReason(session: TerminalSession): string | undefined {
  switch (session.closed_reason) {
    case "user_closed":
      // Anyone who may manage it can close it; only the owner's own close is certainly "you".
      return session.role === "owner" ? "Closed by you" : "Closed"
    case "host_closed":
      return "Ended on the machine"
    case "exited":
      return "Program exited"
    case "host_gone":
      return "Gone from the machine"
    case "unreachable_timeout":
      return "Machine unreachable for 30 days"
  }
}

function Row({
  session,
  time: used,
  now,
  showMachine,
  showRunning,
}: {
  session: TerminalSession
  time: number
  now: number
  showMachine: boolean
  showRunning: boolean
}) {
  const queryClient = useQueryClient()
  const [closing, setClosing] = useState(false)
  const open = terminalCanAttach(session)
  // Open, but its machine isn't reporting it: nothing to attach to until the machine is back.
  const away = open && session.state === "unavailable"
  const offline = session.target.status === "offline"
  const folder = session.current_directory || session.started_in
  // What's running: the app's own window title, or the foreground program.
  const activity = session.title || session.foreground_process
  const href = `/terminals/${encodeURIComponent(session.id)}`
  const time = useTimeFormat()
  const lastUsedLabel =
    used && now > 0
      ? now - used < day
        ? formatRelative(new Date(used).toISOString(), now)
        : time.date(new Date(used).toISOString(), "day")
      : ""

  const close: Entry[] = session.can_manage
    ? [{ kind: "item", label: "Close terminal", icon: PowerIcon, destructive: true, onSelect: () => setClosing(true) }]
    : []
  const entries: Entry[] = away
    ? close
    : open
      ? [
          {
            kind: "item",
            label: "Open in new tab",
            icon: ArrowSquareOutIcon,
            onSelect: () => window.open(href, "_blank", "noopener"),
          },
          // Terminals shared only through a browser link can't be joined from the CLI.
          ...(session.browser_only
            ? []
            : ([
                {
                  kind: "item",
                  label: "Continue in local terminal",
                  icon: TerminalWindowIcon,
                  onSelect: () => void copyWithToast(localCommand(session), "Command", "Run it in your terminal app."),
                },
              ] satisfies Entry[])),
          ...(close.length ? ([{ kind: "separator" }, ...close] satisfies Entry[]) : []),
        ]
      : []

  const row = (actions: React.ReactNode) => (
    <tr
      className={`group relative [&>td]:h-14 [&>td]:px-3 ${open && !away ? "transition-colors hover:bg-surface data-popup-open:bg-surface" : ""}`}
    >
      <td>
        <div className="flex min-w-0 items-center gap-2">
          {open && !away ? (
            // Terminals open in their own tab, like a meeting from a calendar.
            <a
              href={href}
              target="_blank"
              rel="noopener"
              className="min-w-0 truncate-x font-medium trim-cap after:absolute after:inset-0"
            >
              {session.name}
            </a>
          ) : (
            // A closed terminal's shell is gone, and an unreachable one can't be opened until its machine is back.
            <span className="min-w-0 truncate-x font-medium text-muted-foreground trim-cap">{session.name}</span>
          )}
          {/* Someone else's terminal; your own need no label. */}
          {session.role !== "owner" && (
            <Badge variant="outline" className="font-normal text-muted-foreground trim-cap">
              {session.role === "viewer" ? "Shared with you · view only" : "Shared with you"}
            </Badge>
          )}
        </div>
      </td>
      {showMachine && <td className="hidden truncate md:table-cell">{session.target.name}</td>}
      {/* Unknown stays blank: what an unreachable terminal runs isn't known while its machine is away. */}
      {showRunning && <td className="hidden truncate text-muted-foreground lg:table-cell">{away ? "" : activity}</td>}
      <td className="hidden truncate font-mono text-code text-muted-foreground lg:table-cell">{folder}</td>
      <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
        {open ? (
          <TerminalStatus session={session} now={now} />
        ) : closedReason(session) ? (
          <span className="flex flex-col">
            <span>{closedReason(session)}</span>
            <span className="text-xs">{lastUsedLabel}</span>
          </span>
        ) : (
          lastUsedLabel
        )}
      </td>
      <td className="text-right">{actions}</td>
    </tr>
  )

  if (!entries.length) return row(null)
  return (
    <RowMenus
      label={`Actions for ${session.name}`}
      entries={entries}
      row={row}
      dialogs={
        <ConfirmAction
          open={closing}
          onOpenChange={setClosing}
          title={`Close ${session.name}?`}
          description={
            away && offline
              ? `${session.target.name} is offline. The terminal moves to Closed now, and its shell ends when ${session.target.name} reconnects.`
              : `The shell on ${session.target.name} ends for everyone, along with anything running in it.`
          }
          action="Close terminal"
          pending="Closing…"
          mutation={{
            mutationFn: () => closeTerminal(session.target.machine_id, session.id),
            onSuccess: async () => {
              await queryClient.invalidateQueries({ queryKey: terminalKeys.list })
              toast.success(`${session.name} closed`, {
                description: away && offline ? `Its shell ends when ${session.target.name} reconnects.` : undefined,
              })
            },
          }}
        />
      }
    />
  )
}

/**
 * A group heading's cell, spanning just the columns shown at each width (Terminal and the "…" on a
 * phone, then Status, Machine, Folder and Running as they appear). One cell spanning every column,
 * hidden ones included, gave the table phantom columns that squeezed the names to "smok…" on a phone.
 */
function SpanShown({ scope, running, children }: { scope: boolean; running: boolean; children: React.ReactNode }) {
  const md = 3 + (scope ? 0 : 1)
  const spans: [string, number][] = [
    ["sm:hidden", 2],
    ["hidden sm:table-cell md:hidden", 3],
    ["hidden md:table-cell lg:hidden", md],
    ["hidden lg:table-cell", md + 1 + (running ? 1 : 0)],
  ]
  return spans.map(([display, span]) => (
    <th key={display} colSpan={span} className={cn("px-3 pt-4 pb-2 text-left text-xs font-normal", display)}>
      {children}
    </th>
  ))
}
