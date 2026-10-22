"use client"

import { ArrowClockwiseIcon } from "@phosphor-icons/react"
import { useEffect, useState } from "react"
import { toast } from "sonner"
import type { Capture } from "@/lib/inspector/feed"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"

/** Path and query of a captured URL (the daemon may send an absolute URL or a path). */
export function pathOf(url: string) {
  try {
    const parsed = new URL(url, "http://capture.invalid")
    return parsed.pathname + parsed.search
  } catch {
    return url
  }
}

export function durationOf(record: Capture) {
  const ms = Date.parse(record.finished_at) - Date.parse(record.started_at)
  if (!Number.isFinite(ms) || ms < 0) return "—"
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(2)} s`
}

export function timeOf(record: Capture) {
  const at = new Date(record.started_at)
  return Number.isNaN(at.getTime()) ? "—" : at.toLocaleTimeString(undefined, { hour12: false })
}

export function statusTone(status: number) {
  if (status >= 500) return "text-destructive"
  if (status >= 400) return "text-amber-600 dark:text-amber-400"
  return "text-foreground"
}

/** One row of the request list. */
export function CaptureRow({
  record,
  replay,
  selected,
  onSelect,
}: {
  record: Capture
  replay: boolean
  selected: boolean
  onSelect: () => void
}) {
  return (
    <tr
      data-capture-id={record.id}
      aria-selected={selected}
      onClick={onSelect}
      className={`cursor-pointer [&>td]:h-10 [&>td]:px-3 ${selected ? "bg-surface" : "hover:bg-surface/60"}`}
    >
      <td className="font-mono text-code font-medium">{record.method}</td>
      <td className="truncate font-mono text-code">
        {pathOf(record.url)}
        {replay && (
          <Badge variant="outline" className="ml-2 font-sans font-normal text-muted-foreground">
            Replay
          </Badge>
        )}
      </td>
      <td className={`font-mono text-code ${statusTone(record.response_status)}`}>
        {record.response_status || record.error_code || "—"}
      </td>
      <td className="hidden text-right text-muted-foreground sm:table-cell">{durationOf(record)}</td>
      <td className="hidden text-right text-muted-foreground md:table-cell">{timeOf(record)}</td>
    </tr>
  )
}

const bodyStates: Record<string, string> = {
  "": "Not captured. Turn on “Capture bodies” to record bodies.",
  unsupported: "Not captured: this content type isn’t recorded.",
  dropped: "Dropped: the machine was over its capture budget.",
  expired: "Expired.",
}

function Body({ text, state }: { text?: string; state: string }) {
  if (!text) return <p className="text-sm text-muted-foreground">{bodyStates[state] ?? "No body."}</p>
  let shown = text
  try {
    shown = JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    /* Not JSON: shown as captured. */
  }
  return (
    <div className="flex flex-col gap-1">
      <pre className="max-h-96 overflow-auto rounded-md bg-surface p-3 font-mono text-code whitespace-pre-wrap break-all">
        {shown}
      </pre>
      {state === "truncated" && <p className="text-xs text-muted-foreground">Truncated at the capture limit.</p>}
    </div>
  )
}

function Headers({ headers, truncated }: { headers: Record<string, string[]> | null; truncated: boolean }) {
  const rows = Object.entries(headers ?? {}).sort(([a], [b]) => a.localeCompare(b))
  if (!rows.length) return <p className="text-sm text-muted-foreground">No headers.</p>
  return (
    <div className="flex flex-col gap-1">
      <table className="w-full table-fixed text-left font-mono text-code">
        <tbody className="divide-y divide-divider">
          {rows.flatMap(([name, values]) =>
            values.map((value, index) => (
              <tr key={`${name}-${index}`} className="[&>td]:py-1 [&>td]:align-top">
                <td className="w-2/5 pr-3 break-all text-muted-foreground">{name}</td>
                <td className="break-all">{value}</td>
              </tr>
            )),
          )}
        </tbody>
      </table>
      {truncated && <p className="text-xs text-muted-foreground">Some headers were not captured (limit reached).</p>}
    </div>
  )
}

function Section({ title, children }: { title: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-2">
      <h3 className="text-xs font-medium text-muted-foreground">{title}</h3>
      {children}
    </section>
  )
}

/** Request and response of one capture, exactly as captured (redacted values stay redacted). */
export function CaptureDetail({
  record,
  load,
  onReplay,
}: {
  record: Capture
  load: (id: string) => Promise<Capture>
  onReplay?: (id: string) => Promise<unknown>
}) {
  const [replaying, setReplaying] = useState(false)
  useEffect(() => {
    void load(record.id).catch(() => undefined)
  }, [load, record.id])

  const replay = async () => {
    if (!onReplay) return
    setReplaying(true)
    try {
      await onReplay(record.id)
      toast.success("Replayed")
    } catch (error) {
      toast.error(error instanceof Error ? `Replay failed: ${error.message}` : "Replay failed")
    } finally {
      setReplaying(false)
    }
  }

  return (
    <div className="flex flex-col gap-6" data-capture-detail={record.id}>
      <div className="flex items-start gap-3">
        <p className="min-w-0 flex-1 font-mono text-code break-all">
          <span className="font-medium">{record.method}</span> {pathOf(record.url)}
          {record.url_truncated && <span className="text-muted-foreground"> (truncated)</span>}
        </p>
        {onReplay && (
          <Button
            size="sm"
            variant="outline"
            disabled={replaying || Boolean(record.replay_ineligible)}
            title={record.replay_ineligible ? `Can’t replay: ${record.replay_ineligible.replaceAll("_", " ")}` : undefined}
            onClick={() => void replay()}
          >
            <ArrowClockwiseIcon />
            {replaying ? "Replaying…" : "Replay"}
          </Button>
        )}
      </div>
      <Section title="Request headers">
        <Headers headers={record.request_headers} truncated={record.headers_truncated} />
      </Section>
      <Section title="Request body">
        <Body text={record.request_body} state={record.request_body_state} />
      </Section>
      <Section
        title={
          <>
            Response{" "}
            <span className={`font-mono ${statusTone(record.response_status)}`}>
              {record.response_status || record.error_code.replaceAll("_", " ") || "pending"}
            </span>{" "}
            · {durationOf(record)}
          </>
        }
      >
        <Headers headers={record.response_headers} truncated={record.headers_truncated} />
      </Section>
      <Section title="Response body">
        <Body text={record.response_body} state={record.response_body_state} />
      </Section>
    </div>
  )
}
