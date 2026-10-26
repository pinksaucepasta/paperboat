"use client"

import { ArrowsOutIcon, LockSimpleIcon, PowerIcon, UploadSimpleIcon, UsersIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { Children, Fragment, useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { ApiError, meQuery, useApiQuery } from "@/lib/api"
import { cn } from "@/lib/utils"
import { closeTerminal, terminalKeys, terminalQuery } from "@/lib/terminal/queries"
import {
  terminalCanAttach,
  useTerminalConnection,
  type HostParticipant,
  type TerminalConnection,
  type TerminalSession,
} from "@/lib/terminal/use-terminal-connection"
import { useTerminalTheme } from "@/lib/terminal/use-terminal-theme"
import type { TerminalTheme } from "@/lib/terminal/themes"
import { Logo } from "@/components/logo"
import { TerminalSurface } from "@/components/terminal/terminal-surface"
import { TerminalThemeMenu } from "@/components/terminal/terminal-theme-menu"
import { TerminalShareDialog } from "@/components/terminal/terminal-share-dialog"
import { UploadDialog } from "@/components/terminal/upload-dialog"
import { PersonAvatar, peopleIn, personName, TerminalPeople } from "@/components/terminal/terminal-people"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

/**
 * A terminal on its own page, outside the console's sidebar: a top bar, the terminal and the
 * People panel. It is what every "open" leads to, and the page you share.
 */
export function TerminalPage({ id }: { id: string }) {
  const [pushed, setPushed] = useState(false)
  const query = useApiQuery(terminalQuery(id, pushed))
  if (query.isPending) {
    return (
      <Frame>
        <TopBar>
          <Skeleton className="h-4 w-32" />
        </TopBar>
        <div className="flex-1 bg-muted/40" />
      </Frame>
    )
  }
  // A failed refresh (console API down, server restart) keeps the open terminal: only a first load that failed, or a
  // terminal that is gone or no longer shared with you (403/404), replaces it.
  const missing = query.isError && query.error instanceof ApiError && [403, 404].includes(query.error.status)
  if (!query.data || missing) {
    return (
      <Frame>
        <TopBar />
        <main className="flex flex-1 items-center justify-center p-8 text-center">
          <div className="max-w-sm">
            <p className="font-medium">{missing ? "Terminal not found" : "Couldn’t load this terminal"}</p>
            <p className="mt-1 text-sm text-muted-foreground">
              {missing
                ? "It may have closed, or it isn’t shared with you. Ask its owner for access."
                : "Check your connection and try again."}
            </p>
            <Button variant="secondary" className="mt-4" nativeButton={false} render={<Link href="/terminals" />}>
              All terminals
            </Button>
          </div>
        </main>
      </Frame>
    )
  }
  // Mounted once: later polls update the panel and title without restarting the terminal.
  // Keyed by whether it can attach: a session first read as closed or not yet running (just
  // created) gets a fresh connection when a later poll shows it running, instead of staying closed.
  return <TerminalRoom key={String(terminalCanAttach(query.data))} session={query.data} onPushed={setPushed} />
}

export function Frame({ children }: { children: React.ReactNode }) {
  return <div className="flex h-svh min-h-0 flex-col bg-background">{children}</div>
}

/**
 * The terminal page's only chrome, built like the console's page header: a breadcrumb on the
 * left (the boat, then where this terminal lives) and the actions on the right.
 */
export function TopBar({ children, actions }: { children?: React.ReactNode; actions?: React.ReactNode }) {
  return (
    <header className="flex h-(--header-height) shrink-0 items-center gap-2 border-b px-4 md:px-6">
      <Breadcrumb className="min-w-0 flex-1">
        <BreadcrumbList className="flex-nowrap text-sm">
          <BreadcrumbItem>
            <BreadcrumbLink aria-label="Paperboat" className="text-foreground" render={<Link href="/" />}>
              <Logo className="h-[18px]" />
            </BreadcrumbLink>
          </BreadcrumbItem>
          {Children.toArray(children).map((crumb, index) => (
            <Fragment key={index}>
              <BreadcrumbSeparator />
              <BreadcrumbItem className="min-w-0">{crumb}</BreadcrumbItem>
            </Fragment>
          ))}
        </BreadcrumbList>
      </Breadcrumb>
      {actions}
    </header>
  )
}

/** An icon button with its name in a tooltip, the size of the header's other controls. */
function Action({
  label,
  onClick,
  disabled,
  destructive,
  children,
}: {
  label: string
  onClick: () => void
  disabled?: boolean
  destructive?: boolean
  children: React.ReactNode
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            size="icon"
            variant="ghost"
            aria-label={label}
            disabled={disabled}
            onClick={onClick}
            className={cn(
              destructive ? "text-destructive hover:text-destructive" : "text-muted-foreground hover:text-foreground",
            )}
          />
        }
      >
        {children}
      </TooltipTrigger>
      <TooltipContent side="bottom">{label}</TooltipContent>
    </Tooltip>
  )
}

/**
 * The terminal with its bar and People panel. `guest` is someone who joined with a code: no
 * account, so nothing here may call account APIs (they would send a guest to sign-in).
 */
export function TerminalRoom({
  session: polled,
  guest = false,
  onPushed,
}: {
  session: TerminalSession
  guest?: boolean
  /** Told whether the host pushes participants, so /sharing can poll less. */
  onPushed?: (pushed: boolean) => void
}) {
  const router = useRouter()
  const { theme } = useTerminalTheme()
  const hostRef = useRef<HTMLDivElement>(null)
  const connection = useTerminalConnection({ session: polled, theme, hostRef, accessMethod: guest ? "code" : "account" })
  const session = useLiveParticipants(polled, connection.participants, onPushed)
  const { data: me } = useApiQuery({ ...meQuery, enabled: !guest })
  const isOwner = session.role === "owner"
  const people = peopleIn(session)
  // People opens by default once someone else is involved; a terminal of your own starts clean.
  const [peopleOpen, setPeopleOpen] = useState(session.participants_available && (!isOwner || people.length > 1))
  const [shareOpen, setShareOpen] = useState(false)
  const [closing, setClosing] = useState(false)
  const [uploading, setUploading] = useState(false)

  useEffect(() => {
    document.title = `${session.name} · Paperboat`
  }, [session.name])

  return (
    <Frame>
      <TopBar
        actions={
          <div className="flex items-center gap-1">
            {/* Terminal tools, then who's here, then Share: the primary action sits last. */}
            {isOwner && (
              <Action
                label="Upload files"
                disabled={connection.phase !== "attached" || connection.upload.busy}
                onClick={() => setUploading(true)}
              >
                <UploadSimpleIcon />
              </Action>
            )}
            {connection.role !== "viewer" && (
              <Action
                label="Use my window size"
                disabled={connection.phase !== "attached"}
                onClick={connection.fitToWindow}
              >
                <ArrowsOutIcon />
              </Action>
            )}
            <TerminalThemeMenu size="icon" />
            {session.can_manage && (
              <Action label="Close terminal" destructive onClick={() => setClosing(true)}>
                <PowerIcon />
              </Action>
            )}
            <span className="mx-1.5 h-5 w-px bg-border" aria-hidden />
            <button
              type="button"
              onClick={() => setPeopleOpen(!peopleOpen)}
              aria-pressed={peopleOpen}
              aria-label={`People (${people.length})`}
              className={cn(
                "flex h-8 items-center gap-1.5 rounded-md pr-2.5 pl-1 text-sm outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30",
                peopleOpen && "bg-accent",
                people.length === 0 && "pl-2.5",
              )}
            >
              {people.length > 0 ? (
                <span className="flex -space-x-1 *:ring-2 *:ring-background">
                  {people.slice(0, 3).map((person) => (
                    <PersonAvatar
                      key={person.accountId}
                      accountId={person.accountId}
                      name={personName(person, session, me?.id)}
                    />
                  ))}
                </span>
              ) : (
                <UsersIcon className="size-4" />
              )}
              <span className="text-muted-foreground tabular-nums">{people.length}</span>
            </button>
            {isOwner && (
              <Button className="ml-1" onClick={() => setShareOpen(true)}>
                Share
              </Button>
            )}
          </div>
        }
      >
        {!guest && (
          <BreadcrumbLink className="truncate" render={<Link href="/terminals" />}>
            Terminals
          </BreadcrumbLink>
        )}
        {/* Your machine opens its page; someone else's narrows your Terminals list to it. */}
        {!guest &&
          (isOwner ? (
            <BreadcrumbLink
              className="truncate"
              render={<Link href={`/machines/${encodeURIComponent(session.target.machine_id)}/terminals`} />}
            >
              {session.target.name}
            </BreadcrumbLink>
          ) : (
            <BreadcrumbLink
              className="truncate"
              render={<Link href={`/terminals?machine=${encodeURIComponent(session.target.machine_id)}`} />}
            >
              {session.target.name}
            </BreadcrumbLink>
          ))}
        <span className="flex min-w-0 items-baseline gap-2">
          <BreadcrumbPage className="truncate font-medium">{session.name}</BreadcrumbPage>
          {guest ? (
            <span className="hidden shrink-0 text-muted-foreground sm:inline">on {session.target.name} · Guest</span>
          ) : (
            !isOwner && <span className="hidden shrink-0 text-muted-foreground sm:inline">Shared with you</span>
          )}
        </span>
      </TopBar>

      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col">
          <TerminalSurface
            connection={connection}
            hostRef={hostRef}
            theme={theme}
            machineName={session.target.name}
            onLeave={{ label: "All terminals", action: () => router.push("/terminals") }}
            className="flex-1"
          />
          <StatusLine session={session} connection={connection} theme={theme} />
        </div>
        {peopleOpen && <TerminalPeople session={session} meId={me?.id} onClose={() => setPeopleOpen(false)} />}
      </div>

      {isOwner && (
        <UploadDialog
          machineName={session.target.name}
          open={uploading}
          onOpenChange={setUploading}
          onFiles={(files) => void connection.upload.start(files)}
        />
      )}
      {isOwner && <TerminalShareDialog session={session} open={shareOpen} onOpenChange={setShareOpen} />}
      {closing && <CloseTerminal session={session} onClose={() => setClosing(false)} />}
    </Frame>
  )
}

/**
 * One quiet line under the terminal, in its colours: where the shell is and what's running,
 * and the encryption. State and role appear only when they matter: a problem, or watch-only.
 */
function StatusLine({
  session,
  connection,
  theme,
}: {
  session: TerminalSession
  connection: TerminalConnection
  theme: TerminalTheme
}) {
  const { phase, role, connectedEdge, title } = connection
  const live = phase === "attached"
  const problem =
    phase === "reconnecting"
      ? "Reconnecting"
      : phase === "closed"
        ? "Closed"
        : phase === "denied"
          ? "No access"
          : phase === "transport-error"
            ? "Disconnected"
            : phase === "limit"
              ? "Limit reached"
              : null
  // The shell's window title usually names the directory too (anvit@hp: ~/go), and it arrives at
  // once; the directory the server reports lags by a poll, so it's only the fallback.
  const place =
    title ||
    session.title ||
    [session.current_directory || session.started_in, session.foreground_process].filter(Boolean).join(" · ")
  return (
    <div
      className="flex h-7 shrink-0 items-center gap-3 border-t px-3 font-mono text-[11px]"
      style={{
        background: theme.background,
        color: `color-mix(in oklab, ${theme.foreground} 60%, transparent)`,
        borderColor: `color-mix(in oklab, ${theme.foreground} 10%, transparent)`,
      }}
    >
      {problem && <span style={{ color: theme.ansi[3] }}>{problem}</span>}
      {role === "viewer" && <span>Watching only</span>}
      <span className="min-w-0 flex-1 truncate">{place}</span>
      {live && (
        <span
          className="hidden items-center gap-1 sm:flex"
          title={connectedEdge ? `Relayed by ${connectedEdge}` : undefined}
        >
          <LockSimpleIcon className="size-3" />
          End-to-end encrypted
        </span>
      )}
    </div>
  )
}

function CloseTerminal({ session, onClose }: { session: TerminalSession; onClose: () => void }) {
  const router = useRouter()
  const queryClient = useQueryClient()
  const close = useMutation({
    mutationFn: () => closeTerminal(session.target.machine_id, session.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: terminalKeys.list })
      toast.success(`${session.name} closed`)
      router.push("/terminals")
    },
  })
  return (
    <AlertDialog open onOpenChange={(next) => !next && !close.isPending && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Close {session.name}?</AlertDialogTitle>
          <AlertDialogDescription>
            The shell on {session.target.name} ends for everyone, along with anything running in it.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {close.isError && <p className="text-sm text-destructive">{close.error.message}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={close.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={close.isPending}
            onClick={(event) => {
              event.preventDefault()
              close.mutate()
            }}
          >
            {close.isPending ? "Closing…" : "Close terminal"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/**
 * The session with the host's pushed participants when there are any: the push says who is
 * attached, /sharing says what to call them. Someone the polled data doesn't know yet triggers one
 * refresh of /sharing.
 */
function useLiveParticipants(
  session: TerminalSession,
  pushed: HostParticipant[] | null,
  onPushed?: (pushed: boolean) => void,
): TerminalSession {
  const queryClient = useQueryClient()
  const asked = useRef(new Set<string>())
  useEffect(() => onPushed?.(pushed !== null), [pushed, onPushed])
  useEffect(() => {
    const unknown = pushed?.filter((p) => !session.participants.some((s) => s.account_id === p.account_id))
    const fresh = unknown?.filter((p) => !asked.current.has(p.account_id)) ?? []
    if (!fresh.length) return
    fresh.forEach((p) => asked.current.add(p.account_id))
    void queryClient.invalidateQueries({ queryKey: terminalKeys.session(session.id) })
  }, [pushed, session, queryClient])
  if (!pushed) return session
  const participants = pushed.map((p) => {
    const known = session.participants.find((s) => s.account_id === p.account_id)
    return { kind: "member" as const, ...known, ...p }
  })
  return { ...session, participants }
}
