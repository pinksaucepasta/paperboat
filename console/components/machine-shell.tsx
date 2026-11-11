"use client"

import { ShareNetworkIcon, TerminalIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useParams, usePathname, useRouter, useSearchParams, useSelectedLayoutSegment } from "next/navigation"
import { createContext, useContext, useEffect, useState, useSyncExternalStore } from "react"
import { meQuery, useApiQuery, useMachine, type Machine } from "@/lib/api"
import { formatPlatform } from "@/lib/format"
import { machineTabs } from "@/lib/sections"
import { useWorkspaceId } from "@/lib/workspace"
import { cn } from "@/lib/utils"
import { MachineAddress } from "@/components/machine-address"
import { MachinePicker } from "@/components/machine-picker"
import { MachineTitle } from "@/components/machine-identity"
import { MachineTags } from "@/components/machine-tags"
import { MachineStatus } from "@/components/machine-status"
import { CrumbLink, PageHeader } from "@/components/page-header"
import { TeamsUpgradeDialog } from "@/components/team/teams-upgrade"
import { MinimizedTerminal, QuickTerminal, useTerminalSession, type TerminalDock } from "@/components/quick-terminal"
import { useIsMobile } from "@/hooks/use-mobile"
import { Button } from "@/components/ui/button"
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable"
import { Skeleton } from "@/components/ui/skeleton"
import { PageTransition, SharedName } from "@/components/view-transitions"

const MachineContext = createContext<Machine | null>(null)
// True under MachineSkeleton: the machine is a stand-in, so views draw their loading state.
const MachineLoading = createContext(false)

export function useMachineLoading(): boolean {
  return useContext(MachineLoading)
}

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
  // In Personal, sharing your machine with people is a Teams feature: the button explains it.
  const [sharing, setSharing] = useState(false)
  const me = useApiQuery(meQuery).data
  const personalOwner = useWorkspaceId() === "personal" && Boolean(me) && machine?.owner_account === me?.id
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
        <MachineSkeleton machineId={machineId}>{children}</MachineSkeleton>
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
            <MachineTags machine={machine} />
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted-foreground">
              <MachineStatus machine={machine} />
              <span>
                {formatPlatform(machine.platform)} · {machine.architecture}
              </span>
              <MachineAddress machine={machine} />
            </div>
          </div>
          {personalOwner && (
            <Button variant="outline" size="lg" onClick={() => setSharing(true)}>
              <ShareNetworkIcon data-icon="inline-start" />
              Share
            </Button>
          )}
          <Button variant="secondary" size="lg" onClick={() => setTerminal("open")} disabled={terminal === "open"}>
            <TerminalIcon data-icon="inline-start" />
            Quick terminal
          </Button>
        </div>
        <MachineTabs base={base} tab={tab} />
      </div>
      <div className="mx-auto w-full max-w-6xl px-4 py-6 md:px-8">{children}</div>
    </div>
  )

  return (
    <MachineContext value={machine}>
      {header}
      <PageTransition>
        <div className="min-h-0 flex-1">
          {terminal === "open" && !isMobile ? (
            <ResizablePanelGroup
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
      </PageTransition>
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
      <TeamsUpgradeDialog reason="machine" open={sharing} onOpenChange={setSharing} />
    </MachineContext>
  )
}

/* The divider is on a wrapper and the row overlaps it by 1px, so the active underline sits on the
   line without the tabs overflowing (which, with sideways scrolling on, made the row scroll vertically too). */
function MachineTabs({ base, tab }: { base?: string; tab?: string }) {
  const tabClass = (slug: string) =>
    cn(
      "shrink-0 border-b-2 border-transparent px-2.5 pb-2.5 text-sm text-muted-foreground transition-colors hover:text-foreground",
      tab === slug && "border-foreground text-foreground",
    )
  return (
    <div className="border-b border-divider">
      <nav className="-mb-px no-scrollbar flex gap-1 overflow-x-auto overflow-y-hidden">
        {machineTabs.map(({ slug, title }) =>
          // Without a machine yet (the prerendered shell) the tabs keep their place as labels.
          base === undefined ? (
            <span key={slug} className={tabClass(slug)}>
              {title}
            </span>
          ) : (
            <Link key={slug} href={slug ? `${base}/${slug}` : base} className={tabClass(slug)}>
              {title}
            </Link>
          ),
        )}
      </nav>
    </div>
  )
}

/**
 * The loaded header's title and detail lines (their line heights) and Quick terminal (always there;
 * Share, for owners, sits left of it) over the real tabs.
 */
export function MachineSkeleton({ machineId, children }: { machineId?: string; children: React.ReactNode }) {
  // The open tab comes from the route, so the prerendered shell knows it before the machine id.
  const tab = useSelectedLayoutSegment() ?? ""
  // The tab draws its own loading state: its lists only need the machine's id (and read the rest
  // defensively); Overview checks useMachineLoading.
  // Before the id is known (the prerendered shell) a stand-in id still draws the machine-scoped columns.
  const loading = { id: machineId ?? "pending", alias: "", tags: [] } as unknown as Machine
  const personal = useWorkspaceId() === "personal"
  // A machine the list already has shows its name at once, so the list row's name moves into it.
  const routeId = useParams<{ machineId?: string }>().machineId
  const id = machineId ?? routeId
  const alias = useMachine(id).data?.alias
  return (
    <>
      <PageTransition>
        <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-4 pt-6 md:px-8 md:pt-8">
          <div className="flex flex-wrap items-start gap-4">
            <div className="flex min-w-0 flex-1 flex-col gap-2">
              {alias ? (
                <SharedName name={`machine-${id}`}>
                  <h1 className="truncate font-heading text-2xl font-semibold tracking-tight">{alias}</h1>
                </SharedName>
              ) : (
                <Skeleton className="my-1 h-6 w-48" />
              )}
              {/* The tags line, then the status, platform and address line, each value sized like
                itself, so on a phone it wraps like the loaded one ("Last seen 5 days ago" takes
                two lines there). */}
              <Skeleton className="my-0.5 h-4 w-28" />
              <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
                <span className="inline-flex items-center gap-2">
                  <Skeleton className="size-2 rounded-full" />
                  <span className="inline-flex flex-wrap gap-x-1 gap-y-[11px]">
                    <Skeleton className="h-[11px] w-20" />
                    <Skeleton className="h-[11px] w-16" />
                  </span>
                </span>
                <Skeleton className="my-[3px] h-4 w-24" />
                <Skeleton className="my-0.5 h-4 w-28" />
              </div>
            </div>
            {/* Share is there for your own machines in Personal, the usual case. */}
            {personal && (
              <Button variant="outline" size="lg" tabIndex={-1} aria-hidden className="pointer-events-none">
                <ShareNetworkIcon data-icon="inline-start" />
                Share
              </Button>
            )}
            <Button variant="secondary" size="lg" tabIndex={-1} aria-hidden className="pointer-events-none">
              <TerminalIcon data-icon="inline-start" />
              Quick terminal
            </Button>
          </div>
          <MachineTabs base={machineId && `/machines/${machineId}`} tab={tab} />
        </div>
      </PageTransition>
      <PageTransition>
        <div className="mx-auto w-full max-w-6xl px-4 py-6 md:px-8">
          <MachineContext value={loading}>
            <MachineLoading value>{children}</MachineLoading>
          </MachineContext>
        </div>
      </PageTransition>
    </>
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
