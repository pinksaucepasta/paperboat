"use client"

import { ArrowSquareOutIcon, PowerIcon, TerminalIcon, TerminalWindowIcon } from "@phosphor-icons/react"
import { useQueryClient } from "@tanstack/react-query"
import { Fragment, useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { useApiQuery, type Machine } from "@/lib/api"
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
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { copyWithToast } from "@/lib/clipboard"
import { useTimeFormat } from "@/lib/use-time-format"

const day = 86_400_000
/** Closed terminals are kept for 30 days after closing (`expires_at`), which dates the close. */
const retention = 30 * day

/**
 * When a terminal was last in use: open ones are current (the server's list carries no
 * participants or reliable activity time for them), closed ones were last used at
 * `last_active_at`, or 30 days before they expire.
 */
function lastUsed(session: TerminalSession, now: number): number {
  if (terminalCanAttach(session)) return now
  if (session.last_active_at) return Date.parse(session.last_active_at)
  if (session.expires_at) return Date.parse(session.expires_at) - retention
  return 0
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

  // Open terminals are all current, so they're one list; closed ones group by when they were used.
  const columns = 4 + (scope ? 0 : 1) + (view === "open" ? 1 : 0)
  const groups: { key: string; label: string; rows: typeof shown }[] = []
  if (view === "open" && shown.length) groups.push({ key: "open", label: "", rows: shown })
  else
    for (const row of shown) {
      const { key, label } = groupOf(row.time, now, date)
      const group = groups.at(-1)
      if (group?.key === key) group.rows.push(row)
      else groups.push({ key, label, rows: [row] })
    }

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
      <div className="flex items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search terminals" />
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
            {view === "open" && <col className="hidden w-[22%] lg:table-column" />}
            <col className="hidden w-[24%] lg:table-column" />
            <col className="hidden w-[14%] sm:table-column" />
            <col className="w-12" />
          </colgroup>
          <thead className="border-b border-divider text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Terminal</th>
              {!scope && <th className="hidden md:table-cell">Machine</th>}
              {view === "open" && <th className="hidden lg:table-cell">Running</th>}
              <th className="hidden lg:table-cell">Folder</th>
              <th className="hidden sm:table-cell">{view === "open" ? "Status" : "Last used"}</th>
              <th className="w-12" aria-label="Actions" />
            </tr>
          </thead>
          {query.isPending ? (
            <tbody>
              {Array.from({ length: 4 }, (_, index) => (
                <tr key={index} className="[&>td]:h-14 [&>td]:px-3">
                  <td colSpan={columns}>
                    <Skeleton className="h-5 w-1/3" />
                  </td>
                </tr>
              ))}
            </tbody>
          ) : (
            groups.map((group) => (
              <Fragment key={group.key}>
                {group.label && (
                  <tbody>
                    <tr>
                      <th colSpan={columns} className="h-11 px-3 pt-4 text-left text-xs font-medium text-foreground">
                        {group.label}
                      </th>
                    </tr>
                  </tbody>
                )}
                <tbody className="divide-y divide-divider border-b border-divider">
                  {group.rows.map(({ session }) => (
                    <Row key={session.id} session={session} now={now} showMachine={!scope} />
                  ))}
                </tbody>
              </Fragment>
            ))
          )}
        </table>
        <ListPager page={pages.page} size={terminalPageSize} total={total} go={pages.go} />
        {query.isError && (
          <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
            Couldn’t load terminals.
            <Button variant="outline" size="sm" onClick={() => query.refetch()}>
              Retry
            </Button>
          </div>
        )}
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
 * Unavailable means the machine isn’t reporting it, usually because it’s offline.
 */
function TerminalStatus({ session }: { session: TerminalSession }) {
  if (session.state === "attached")
    return (
      <span className="flex items-center gap-2">
        <StatusDot online />
        In use
      </span>
    )
  if (session.state === "unavailable") return <span className="text-muted-foreground">Unavailable</span>
  return (
    <span className="flex items-center gap-2">
      <StatusDot online={false} />
      Idle
    </span>
  )
}

function Row({ session, now, showMachine }: { session: TerminalSession; now: number; showMachine: boolean }) {
  const queryClient = useQueryClient()
  const [closing, setClosing] = useState(false)
  const open = terminalCanAttach(session)
  const folder = session.current_directory || session.started_in
  // What's running: the app's own window title, or the foreground program.
  const activity = session.title || session.foreground_process
  const href = `/terminals/${encodeURIComponent(session.id)}`
  const time = useTimeFormat()
  const lastUsedLabel =
    session.last_active_at && now > 0
      ? now - Date.parse(session.last_active_at) < day
        ? formatRelative(session.last_active_at, now)
        : time.date(session.last_active_at, "day")
      : "—"

  const entries: Entry[] = open
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
        ...(session.can_manage
          ? ([
              { kind: "separator" },
              {
                kind: "item",
                label: "Close terminal",
                icon: PowerIcon,
                destructive: true,
                onSelect: () => setClosing(true),
              },
            ] satisfies Entry[])
          : []),
      ]
    : []

  const row = (actions: React.ReactNode) => (
    <tr
      className={`group relative [&>td]:h-14 [&>td]:px-3 ${open ? "transition-colors hover:bg-surface data-popup-open:bg-surface" : ""}`}
    >
      <td>
        <div className="flex min-w-0 items-center gap-2">
          {open ? (
            // Terminals open in their own tab, like a meeting from a calendar.
            <a
              href={href}
              target="_blank"
              rel="noopener"
              className="truncate-x font-medium trim-cap after:absolute after:inset-0"
            >
              {session.name}
            </a>
          ) : (
            // A closed terminal's shell is gone; there is nothing to open.
            <span className="truncate-x font-medium text-muted-foreground trim-cap">{session.name}</span>
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
      {open && <td className="hidden truncate text-muted-foreground lg:table-cell">{activity || "—"}</td>}
      <td className="hidden truncate font-mono text-code text-muted-foreground lg:table-cell">{folder || "—"}</td>
      <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
        {open ? <TerminalStatus session={session} /> : lastUsedLabel}
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
          description={`The shell on ${session.target.name} ends for everyone, along with anything running in it.`}
          action="Close terminal"
          pending="Closing…"
          mutation={{
            mutationFn: () => closeTerminal(session.target.machine_id, session.id),
            onSuccess: async () => {
              await queryClient.invalidateQueries({ queryKey: terminalKeys.list })
              toast.success(`${session.name} closed`)
            },
          }}
        />
      }
    />
  )
}
