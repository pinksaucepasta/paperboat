"use client"

import { TerminalIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { createContext, useContext, useEffect, useState, useSyncExternalStore } from "react"
import { useMachine, type Machine } from "@/lib/api"
import { formatPlatform } from "@/lib/format"
import { machineTabs } from "@/lib/sections"
import { cn } from "@/lib/utils"
import { MachineAddress } from "@/components/machine-address"
import { MachinePicker } from "@/components/machine-picker"
import { MachineTitle } from "@/components/machine-identity"
import { MachineStatus } from "@/components/machine-status"
import { CrumbLink, PageHeader } from "@/components/page-header"
import { MinimizedTerminal, QuickTerminal, useTerminalSession, type TerminalDock } from "@/components/quick-terminal"
import { useIsMobile } from "@/hooks/use-mobile"
import { Button } from "@/components/ui/button"
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable"
import { Skeleton } from "@/components/ui/skeleton"

const MachineContext = createContext<Machine | null>(null)

export function useCurrentMachine(): Machine {
  const machine = useContext(MachineContext)
  if (!machine) throw new Error("useCurrentMachine must be used inside MachineShell")
  return machine
}

export function MachineShell({ machineId, children }: { machineId: string; children: React.ReactNode }) {
  const router = useRouter()
  const pathname = usePathname()
  const { data: machine, isPending, isError, refetch } = useMachine(machineId)
  // `?terminal=open` (the list's "Quick terminal") opens the terminal whenever it arrives, even
  // on a page Next kept alive from an earlier visit; the parameter is then removed.
  const searchParams = useSearchParams()
  const terminalRequested = searchParams.get("terminal") === "open"
  const [terminal, setTerminal] = useState<"closed" | "open" | "minimized">("closed")
  const [handledRequest, setHandledRequest] = useState(false)
  if (terminalRequested && !handledRequest) {
    setHandledRequest(true)
    setTerminal("open")
  }
  if (!terminalRequested && handledRequest) setHandledRequest(false)
  useEffect(() => {
    if (terminalRequested) router.replace(pathname, { scroll: false })
  }, [terminalRequested, pathname, router])
  const session = useTerminalSession(machine, terminal !== "closed")
  const dock = useTerminalDock()
  const isMobile = useIsMobile()

  const base = `/machines/${machineId}`
  const tab = pathname.slice(base.length).replace(/^\//, "").split("/")[0] ?? ""
  // Switching machine keeps you on the same tab.
  const switchMachine = (id: string | null) => id && router.push(`/machines/${id}${tab ? `/${tab}` : ""}`)

  const header = (
    <PageHeader>
      <CrumbLink href="/">Machines</CrumbLink>
      <MachinePicker value={machineId} onChange={switchMachine} />
    </PageHeader>
  )

  if (isPending) {
    return (
      <>
        {header}
        <MachineSkeleton />
      </>
    )
  }
  if (isError || !machine) {
    return (
      <>
        {header}
        <div className="flex flex-col items-center gap-4 py-24 text-center text-sm text-muted-foreground">
          {isError ? "Couldn’t load this machine." : "This machine doesn’t exist or isn’t shared with you."}
          {isError ? (
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              Retry
            </Button>
          ) : (
            <Button variant="outline" size="sm" render={<Link href="/" />}>
              All machines
            </Button>
          )}
        </div>
      </>
    )
  }

  const content = (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-4 pt-6 md:px-8 md:pt-8">
        <div className="flex flex-wrap items-start gap-4">
          <div className="flex min-w-0 flex-1 flex-col gap-2">
            <MachineTitle machine={machine} />
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted-foreground">
              <MachineStatus machine={machine} />
              <span>
                {formatPlatform(machine.platform)} · {machine.architecture}
              </span>
              <MachineAddress machine={machine} />
            </div>
          </div>
          <Button variant="secondary" size="lg" onClick={() => setTerminal("open")} disabled={terminal === "open"}>
            <TerminalIcon data-icon="inline-start" />
            Quick terminal
          </Button>
        </div>
        {/* The divider is on a wrapper and the row overlaps it by 1px, so the active underline
            sits on the line without the tabs overflowing (which, with sideways scrolling on,
            made the row scroll vertically too). */}
        <div className="border-b border-divider">
          <nav className="-mb-px no-scrollbar flex gap-1 overflow-x-auto overflow-y-hidden">
            {machineTabs.map(({ slug, title }) => (
              <Link
                key={slug}
                href={slug ? `${base}/${slug}` : base}
                className={cn(
                  "shrink-0 border-b-2 border-transparent px-2.5 pb-2.5 text-sm text-muted-foreground transition-colors hover:text-foreground",
                  tab === slug && "border-foreground text-foreground",
                )}
              >
                {title}
              </Link>
            ))}
          </nav>
        </div>
      </div>
      <div className="mx-auto w-full max-w-6xl px-4 py-6 md:px-8">{children}</div>
    </div>
  )

  return (
    <MachineContext value={machine}>
      {header}
      <div className="min-h-0 flex-1">
        {terminal === "open" && !isMobile ? (
          <ResizablePanelGroup
            key={dock.value}
            orientation={dock.value === "bottom" ? "vertical" : "horizontal"}
            className="h-[calc(100svh-var(--header-height))]"
          >
            <ResizablePanel minSize="30%">{content}</ResizablePanel>
            <ResizableHandle />
            <ResizablePanel defaultSize="35%" minSize="15%">
              <QuickTerminal
                machine={machine}
                session={session}
                dock={dock.value}
                onDock={dock.set}
                onMinimize={() => setTerminal("minimized")}
                onClose={() => setTerminal("closed")}
              />
            </ResizablePanel>
          </ResizablePanelGroup>
        ) : (
          content
        )}
      </div>
      {/* Phones have no room to dock, so the terminal takes the whole screen. */}
      {terminal === "open" && isMobile && (
        <div className="fixed inset-0 z-50 pt-[env(safe-area-inset-top)]">
          <QuickTerminal
            machine={machine}
            session={session}
            onMinimize={() => setTerminal("minimized")}
            onClose={() => setTerminal("closed")}
          />
        </div>
      )}
      {terminal === "minimized" && (
        <MinimizedTerminal
          machine={machine}
          session={session}
          onRestore={() => setTerminal("open")}
          onClose={() => setTerminal("closed")}
        />
      )}
    </MachineContext>
  )
}

function MachineSkeleton() {
  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-4 py-6 md:px-8 md:py-8">
      <div className="flex flex-col gap-3">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-4 w-72" />
      </div>
      <Skeleton className="h-8 w-full" />
      <Skeleton className="h-64 w-full" />
    </div>
  )
}

/** Where the quick terminal docks, remembered in this browser like an editor's panel position. */
const dockKey = "pb-terminal-dock"
const dockListeners = new Set<() => void>()

function useTerminalDock(): { value: TerminalDock; set: (dock: TerminalDock) => void } {
  const value = useSyncExternalStore(
    (listener) => {
      dockListeners.add(listener)
      return () => dockListeners.delete(listener)
    },
    () => {
      try {
        return localStorage.getItem(dockKey) === "right" ? "right" : "bottom"
      } catch {
        return "bottom"
      }
    },
    () => "bottom" as TerminalDock,
  )
  const set = (dock: TerminalDock) => {
    try {
      localStorage.setItem(dockKey, dock)
    } catch {
      // Not remembered without storage; the panel still moves.
    }
    dockListeners.forEach((listener) => listener())
  }
  return { value, set }
}
