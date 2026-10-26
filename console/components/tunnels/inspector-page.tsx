"use client"

import { CaretDownIcon, PauseIcon, PlayIcon, TrashIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useDeferredValue, useEffect, useState } from "react"
import { toast } from "sonner"
import { useApiQuery } from "@/lib/api"
import { useInspectorChannel, type ChannelPhase } from "@/lib/inspector/channel"
import { useCaptureFeed, type Capture } from "@/lib/inspector/feed"
import { hostOf, previewQuery, type Preview } from "@/lib/tunnels"
import { CaptureDetail, CaptureRow, pathOf } from "@/components/tunnels/inspector-detail"
import { ConfirmAction, SearchField, ViewToggle } from "@/components/list-toolbar"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ended } from "@/components/tunnels/preview-list"
import { useNow } from "@/lib/use-now"

type StatusClass = "all" | "2xx" | "3xx" | "4xx" | "5xx"
const METHODS = ["All", "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]

/** Previews → Inspect: a preview's captured requests, live (PLAN F17). */
export function InspectorPage({ id }: { id: string }) {
  const query = useApiQuery({ ...previewQuery(id), refetchInterval: 10_000 })
  const preview = query.data
  const now = useNow(10_000)
  return (
    <>
      <PageHeader>
        <CrumbLink href="/previews">Previews</CrumbLink>
        {preview ? <Crumb>{hostOf(preview.endpoint)}</Crumb> : <Skeleton className="h-4 w-24" />}
        <Crumb>Inspect</Crumb>
      </PageHeader>
      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col gap-4 px-4 py-6 md:px-8">
        {query.isError ? (
          <State title="Preview not found" detail={query.error.message} />
        ) : !preview ? (
          <Skeleton className="h-8 w-72" />
        ) : ended(preview, now) ? (
          <State title="Preview stopped" detail="Captures end with the preview. Start a new preview to inspect it." />
        ) : (
          <Inspector preview={preview} onDenied={query.refetch} />
        )}
      </main>
    </>
  )
}

function State({ title, detail, children }: { title: string; detail: string; children?: React.ReactNode }) {
  return (
    <Empty className="py-16" data-inspector-state={title}>
      <EmptyHeader>
        <EmptyTitle>{title}</EmptyTitle>
        <EmptyDescription>{detail}</EmptyDescription>
      </EmptyHeader>
      {children}
      <Button variant="outline" size="sm" render={<Link href="/previews" />} nativeButton={false}>
        All previews
      </Button>
    </Empty>
  )
}

const phaseText: Record<Exclude<ChannelPhase, "ready">, [string, string]> = {
  connecting: ["Connecting", "Opening an encrypted connection to the machine…"],
  reconnecting: ["Connection lost", "Reconnecting to the machine…"],
  offline: ["Machine offline", "The machine running this preview isn’t connected. Retrying…"],
  denied: ["Not allowed", "You need inspect access to this preview. Ask its owner."],
  update: ["Machine needs an update", "Update Paperboat on this machine (pb update) to inspect its previews."],
  gone: ["Preview not found", "This preview no longer exists."],
}

function Inspector({ preview, onDenied }: { preview: Preview; onDenied: () => unknown }) {
  // The server grants inspection only while the preview routes. While it starts or its machine
  // reconnects, a refusal means 'wait', not 'not allowed'; the list collected so far stays.
  const routing = preview.state === "ready"
  const channel = useInspectorChannel(preview.id, preview.owner_machine_id, true, routing)
  // A refusal right after the owner stops the preview is the stop, not a lost right: re-read the preview now.
  useEffect(() => {
    if (channel.phase === "denied") void onDenied()
  }, [channel.phase, onDenied])
  const [paused, setPaused] = useState(false)
  const feed = useCaptureFeed(channel, preview.id, paused)
  const [selected, setSelected] = useState("")
  const [method, setMethod] = useState("All")
  const [status, setStatus] = useState<StatusClass>("all")
  const [search, setSearch] = useState("")
  const text = useDeferredValue(search.trim().toLowerCase())
  const [clearing, setClearing] = useState(false)

  // Final states replace the page; while reconnecting the last list stays visible under a notice.
  if (!routing && channel.phase !== "ready" && feed.records.length === 0)
    return <State title="Waiting for the preview" detail="The preview is starting or its machine is reconnecting. Inspection continues when it is back." />
  if (channel.phase !== "ready" && (feed.records.length === 0 || ["denied", "update", "gone"].includes(channel.phase))) {
    const [title, detail] = phaseText[channel.phase]
    return <State title={title} detail={channel.phase === "denied" && channel.message ? channel.message : detail} />
  }

  const shown = feed.records.filter(
    (record) =>
      (method === "All" || record.method === method) &&
      (status === "all" || String(record.response_status).startsWith(status[0])) &&
      (!text || pathOf(record.url).toLowerCase().includes(text)),
  )
  const current = feed.records.find((record) => record.id === selected)

  const policy = async (enabled: boolean, bodies: boolean) => {
    try {
      await feed.setPolicy(enabled, bodies)
    } catch (error) {
      toast.error(error instanceof Error ? `Couldn’t change capture: ${error.message}` : "Couldn’t change capture")
    }
  }

  return (
    <section className="flex flex-col gap-3" data-inspector-phase={channel.phase} data-capture={feed.capture}>
      {channel.phase !== "ready" && (
        <p className="text-sm text-muted-foreground" role="status">
          {routing
            ? phaseText[channel.phase][1]
            : "The preview is starting or its machine is reconnecting. Inspection continues when it is back."}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Filter by path" />
        <MethodFilter value={method} onChange={setMethod} />
        <ViewToggle
          value={status}
          onChange={setStatus}
          views={(["all", "2xx", "3xx", "4xx", "5xx"] as const).map((value) => ({
            value,
            label: value === "all" ? "All" : value,
          }))}
        />
        <Button variant="outline" size="sm" onClick={() => setPaused(!paused)}>
          {paused ? <PlayIcon /> : <PauseIcon />}
          {paused ? "Resume" : "Pause"}
        </Button>
        {channel.canReplay && (
          <div className="ml-auto flex items-center gap-4 text-sm">
            <label className="flex items-center gap-2">
              <Switch checked={feed.capture === "on"} onCheckedChange={(on) => void policy(on, feed.bodies)} />
              Capture
            </label>
            <label className="flex items-center gap-2" title="Keep request and response bodies on the machine; needed to replay">
              <Switch
                checked={feed.bodies}
                disabled={feed.capture !== "on"}
                onCheckedChange={(on) => void policy(true, on)}
              />
              Bodies
            </label>
            <Button variant="outline" size="sm" onClick={() => setClearing(true)}>
              <TrashIcon />
              Clear
            </Button>
          </div>
        )}
      </div>
      {feed.capture === "off" && feed.records.length === 0 ? (
        <State
          title="Capture is off"
          detail={
            channel.canReplay
              ? "Turn on Capture to record requests to this preview."
              : "Ask the preview’s owner to turn on capture, or run pb preview inspect on the machine."
          }
        />
      ) : feed.records.length === 0 ? (
        <p className="py-16 text-center text-sm text-muted-foreground" data-inspector-state="No requests yet">
          No requests yet. Open {hostOf(preview.endpoint)} and they appear here.
        </p>
      ) : (
        <Panes
          shown={shown}
          selected={current}
          onSelect={setSelected}
          load={feed.load}
          onReplay={channel.canReplay ? feed.replay : undefined}
        />
      )}
      <ConfirmAction
        open={clearing}
        onOpenChange={setClearing}
        title="Clear captured requests?"
        description="Every captured request for this preview is deleted from the machine."
        action="Clear"
        pending="Clearing…"
        mutation={{ mutationFn: feed.clear, onSuccess: () => setSelected("") }}
      />
    </section>
  )
}

function MethodFilter({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={<Button variant="outline" size="sm" />}>
        {value === "All" ? "Method" : value}
        <CaretDownIcon />
      </DropdownMenuTrigger>
      <DropdownMenuContent>
        <DropdownMenuRadioGroup value={value} onValueChange={(next) => onChange(String(next))}>
          {METHODS.map((entry) => (
            <DropdownMenuRadioItem key={entry} value={entry}>
              {entry}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function Panes({
  shown,
  selected,
  onSelect,
  load,
  onReplay,
}: {
  shown: Capture[]
  selected?: Capture
  onSelect: (id: string) => void
  load: (id: string) => Promise<Capture>
  onReplay?: (id: string) => Promise<unknown>
}) {
  return (
    <div className="grid gap-6 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
      <div className="max-h-[70vh] overflow-auto">
        <table className="w-full table-fixed text-sm" data-inspector-list>
          <colgroup>
            <col className="w-20" />
            <col />
            <col className="w-16" />
            <col className="hidden w-20 sm:table-column" />
            <col className="hidden w-24 md:table-column" />
          </colgroup>
          <thead className="sticky top-0 border-b border-divider bg-background text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Method</th>
              <th>Path</th>
              <th>Status</th>
              <th className="hidden text-right sm:table-cell">Duration</th>
              <th className="hidden text-right md:table-cell">Time</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-divider border-b border-divider">
            {shown.map((record) => (
              <CaptureRow
                key={record.id}
                record={record}
                replay={Boolean(record.replay_of)}
                selected={record.id === selected?.id}
                onSelect={() => onSelect(record.id)}
              />
            ))}
          </tbody>
        </table>
        {shown.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No requests match</p>}
      </div>
      <div className="min-w-0">
        {selected ? (
          <CaptureDetail key={selected.id} record={selected} load={load} onReplay={onReplay} />
        ) : (
          <p className="py-12 text-center text-sm text-muted-foreground">Select a request to see its details</p>
        )}
      </div>
    </div>
  )
}
