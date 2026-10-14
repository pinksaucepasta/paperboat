"use client"

import {
  ArrowSquareOutIcon,
  CaretUpIcon,
  CircleNotchIcon,
  MinusIcon,
  SidebarSimpleIcon,
  SquareSplitVerticalIcon,
  WarningCircleIcon,
  XIcon,
} from "@phosphor-icons/react"
import { useEffect, useRef } from "react"
import { useApiQuery, type Machine } from "@/lib/api"
import { cn } from "@/lib/utils"
import { closeTerminal, terminalQuery } from "@/lib/terminal/queries"
import { useTerminalConnection, type TerminalSession } from "@/lib/terminal/use-terminal-connection"
import { useTerminalTheme } from "@/lib/terminal/use-terminal-theme"
import { canHostTerminal, useStartShell } from "@/components/terminal/new-terminal"
import { TerminalSurface } from "@/components/terminal/terminal-surface"
import { TerminalThemeMenu } from "@/components/terminal/terminal-theme-menu"
import { StatusDot } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

export type TerminalDock = "bottom" | "right"

/** The quick terminal's shell: made when the panel first opens, and ended when it closes. */
export type QuickSession = {
  state: "starting" | "ready" | "unavailable" | "failed"
  session?: TerminalSession
  error?: string
  retry: () => void
}

/**
 * The shell is owned by the page, not the panel, so minimising and restoring reattach to the
 * same shell; closing the panel ends it, like closing a terminal in an editor. "Open in new
 * tab" opens the same shell at /terminals/{id}, where it can be shared.
 */
export function useTerminalSession(machine: Machine | undefined, active: boolean): QuickSession {
  const create = useStartShell()
  const id = create.data?.id
  const details = useApiQuery({ ...terminalQuery(id ?? ""), enabled: Boolean(id) })
  const available = !!machine && canHostTerminal(machine)

  // One shell per opening. A ref, not the mutation's state, guards it: effects can run twice
  // before the mutation reports it has started (React's development double-run does exactly that).
  const started = useRef(false)
  useEffect(() => {
    if (!active) {
      started.current = false
      return
    }
    if (available && !started.current) {
      started.current = true
      create.mutate(machine)
    }
  }, [active, available, create, machine])

  // Closing the panel ends its shell. Leaving the page doesn't:
  // the shell keeps running and is listed under Terminals.
  const session = details.data
  useEffect(() => {
    if (active || !id || !machine) return
    void closeTerminal(machine.id, id).catch(() => {})
    create.reset()
  }, [active, id, machine, create])

  if (!available) return { state: "unavailable", retry: () => {} }
  if (create.isError || details.isError) {
    return {
      state: "failed",
      error: (create.error ?? details.error)?.message,
      retry: () => {
        create.reset()
        if (machine) create.mutate(machine)
      },
    }
  }
  if (!session) return { state: "starting", retry: () => {} }
  return { state: "ready", session, retry: () => {} }
}

export function QuickTerminal({
  machine,
  session,
  dock,
  onDock,
  onMinimize,
  onClose,
}: {
  machine: Machine
  session: QuickSession
  /** Omitted where the terminal cannot dock, such as full screen on phones. */
  dock?: TerminalDock
  onDock?: (dock: TerminalDock) => void
  onMinimize: () => void
  onClose: () => void
}) {
  const live = session.state === "ready" && session.session
  return (
    <div className="flex h-full min-h-0 flex-col bg-sidebar">
      <div className="flex h-9 shrink-0 items-center gap-2 border-b px-3 text-xs">
        <span className="font-medium trim-cap">Terminal</span>
        <span className="text-muted-foreground trim-cap">{machine.alias}</span>
        <div className="ml-auto flex items-center gap-0.5">
          {live && (
            <>
              <TerminalThemeMenu />
              <Tooltip>
                <TooltipTrigger
                  render={
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label="Open in new tab"
                      className="text-muted-foreground"
                      nativeButton={false}
                      render={<a href={`/terminals/${encodeURIComponent(live.id)}`} target="_blank" rel="noopener" />}
                    />
                  }
                >
                  <ArrowSquareOutIcon />
                </TooltipTrigger>
                <TooltipContent side="top">Open in new tab to share</TooltipContent>
              </Tooltip>
            </>
          )}
          {dock && onDock && (
            <>
              <PanelButton active={dock === "bottom"} label="Dock to bottom" onClick={() => onDock("bottom")}>
                <SquareSplitVerticalIcon />
              </PanelButton>
              <PanelButton active={dock === "right"} label="Dock to side" onClick={() => onDock("right")}>
                <SidebarSimpleIcon className="-scale-x-100" />
              </PanelButton>
            </>
          )}
          <PanelButton label="Minimize" onClick={onMinimize}>
            <MinusIcon />
          </PanelButton>
          <PanelButton label="Close terminal" onClick={onClose}>
            <XIcon />
          </PanelButton>
        </div>
      </div>
      {live ? (
        <LiveTerminal session={live} />
      ) : (
        <div className="flex min-h-24 flex-1 flex-col items-center justify-center gap-3 p-4 text-sm text-muted-foreground">
          {session.state === "starting" ? (
            <span className="flex items-center gap-2">
              <CircleNotchIcon className="size-4 animate-spin" />
              Starting a shell on {machine.alias}…
            </span>
          ) : (
            <>
              <span className="flex items-center gap-2">
                <WarningCircleIcon className="size-4" />
                {session.state === "unavailable"
                  ? machine.online
                    ? `${machine.alias} can’t host terminals`
                    : `${machine.alias} is offline`
                  : (session.error ?? "Couldn’t start a shell")}
              </span>
              {session.state === "failed" && (
                <Button variant="outline" size="sm" onClick={session.retry}>
                  Try again
                </Button>
              )}
            </>
          )}
        </div>
      )}
    </div>
  )
}

function LiveTerminal({ session }: { session: TerminalSession }) {
  const { theme } = useTerminalTheme()
  const hostRef = useRef<HTMLDivElement>(null)
  const connection = useTerminalConnection({ session, theme, hostRef })
  return (
    <TerminalSurface
      connection={connection}
      hostRef={hostRef}
      theme={theme}
      machineName={session.target.name}
      className="min-h-0 flex-1"
    />
  )
}

/** What a minimised terminal leaves behind: one bar to bring it back. */
export function MinimizedTerminal({
  machine,
  session,
  onRestore,
  onClose,
}: {
  machine: Machine
  session: QuickSession
  onRestore: () => void
  onClose: () => void
}) {
  return (
    <div className="fixed right-4 bottom-4 z-40 flex items-center gap-1 rounded-lg border bg-popover py-1 pr-1 pl-3 text-sm shadow-lg">
      <button type="button" onClick={onRestore} className="flex items-center gap-2 py-1 pr-1 outline-none">
        <StatusDot online={session.state === "ready"} />
        <span className="font-medium trim-cap">Terminal</span>
        <span className="text-muted-foreground trim-cap">{machine.alias}</span>
      </button>
      <PanelButton label="Restore" onClick={onRestore}>
        <CaretUpIcon />
      </PanelButton>
      <PanelButton label="Close terminal" onClick={onClose}>
        <XIcon />
      </PanelButton>
    </div>
  )
}

function PanelButton({
  active,
  label,
  onClick,
  children,
}: {
  active?: boolean
  label: string
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={label}
            aria-pressed={active}
            onClick={onClick}
            className={cn("text-muted-foreground", active && "bg-accent text-foreground")}
          />
        }
      >
        {children}
      </TooltipTrigger>
      <TooltipContent side="top">{label}</TooltipContent>
    </Tooltip>
  )
}
