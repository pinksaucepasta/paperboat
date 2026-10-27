"use client"

import { CopyIcon, EyeIcon, MagnifyingGlassIcon, StopIcon } from "@phosphor-icons/react"
import { useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { cursorPageSize } from "@/lib/cursor-pages"
import { tunnelKeys, hostOf, originLabel, previewCountsQuery, previewsPageQuery, stopPreview, type Preview } from "@/lib/tunnels"
import { usePageParam } from "@/lib/use-page-param"
import { ListPager } from "@/components/list-pager"
import { formatRelative } from "@/lib/format"
import { useNow } from "@/lib/use-now"
import { NewPreviewButton } from "@/components/tunnels/new-preview"
import { DropOverlay, usePageFileDrop } from "@/components/file-drop"
import { ShareFileButton, ShareFileDialog } from "@/components/tunnels/share-file"
import { ConfirmAction, SearchField, useListScope, ViewToggle } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { StatusDot } from "@/components/machine-status"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { copyWithToast } from "@/lib/clipboard"

type View = "live" | "ended"

/** Stopped, expired, owner lost (`owner_disconnected` is terminal), past its deadline, or the server says its lease ended. */
export const ended = (preview: Preview, now: number) =>
  preview.endpoint_state === "ended" ||
  preview.state === "expired" ||
  preview.state === "stopped" ||
  preview.state === "owner_disconnected" ||
  Date.parse(preview.user_deadline ?? preview.lease_deadline) < now

/**
 * When a preview ends. One started with a time limit ends at it; one tied to a running command
 * (`pb preview 3000` in the foreground) lasts while the command runs, so its short, constantly
 * renewed lease deadline would only look alarming.
 */
function endsLabel(preview: Preview, now: number) {
  if (preview.user_deadline) return formatRelative(preview.user_deadline, now)
  if (preview.owner_session_kind === "foreground") return "When stopped"
  return formatRelative(preview.lease_deadline, now)
}

/** When an ended preview stopped: its last renewal, or its deadline when it ran out. */
function endedAt(preview: Preview) {
  return preview.last_renewed_at ?? preview.user_deadline ?? preview.lease_deadline
}

/**
 * Whether a live preview answers. The address only works once the edge route and the machine's
 * connection are both ready, and then only while the app on the machine is listening.
 */
function PreviewStatus({ preview }: { preview: Preview }) {
  const [online, label] =
    preview.state === "owner_disconnected"
      ? [false, "Machine disconnected"]
      : preview.state !== "ready"
        ? [false, "Starting"]
        : preview.edge_state === "down" || preview.edge_state === "degraded"
          ? [false, "Edge trouble"]
          : preview.origin_state === "unavailable"
            ? [false, "App not responding"]
            : [true, "Live"]
  return (
    <span className="flex items-center gap-2">
      <StatusDot online={online} />
      {label}
    </span>
  )
}

/** The machine page's Previews tab. */
export function MachinePreviews() {
  return <PreviewList machine={useCurrentMachine()} />
}

/**
 * Temporary public addresses for things running on machines (ngrok-style): the address is
 * generated, it lives as long as its command or time limit, and stopping it is the only edit.
 */
export function PreviewList({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const { scope, machine, aliasOf } = useListScope(tabMachine)
  const now = useNow(30_000)
  const [view, setView] = useState<View>("live")
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const ws = workspace?.id ?? ""
  const pages = usePageParam(`${view}|${q}|${scope ?? ""}`)
  const counts = useApiQuery({ ...previewCountsQuery(ws, scope ?? undefined), enabled: Boolean(workspace) })
  const query = useApiQuery({
    ...previewsPageQuery({ workspace: ws, ended: view === "ended", q, page: pages.page, machine: scope ?? undefined }),
    enabled: Boolean(workspace),
  })
  // Dropping a file anywhere on the page shares it (like drop.new).
  const drop = usePageFileDrop()
  const dropped = (
    <>
      {drop.over && !drop.files.length && <DropOverlay title="Drop to share" detail="It gets a link right away" />}
      {drop.files[0] && <ShareFileDialog machine={machine} file={drop.files[0]} onClose={drop.clear} />}
    </>
  )

  const shown = query.data?.items ?? []
  const columns = (view === "live" ? 5 : 4) + (scope ? 0 : 1)

  if (counts.data && counts.data.live + counts.data.ended === 0 && !q) {
    return (
      <Empty className="py-16">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <EyeIcon />
          </EmptyMedia>
          <EmptyTitle>No previews yet</EmptyTitle>
          <EmptyDescription>
            Share a file, or something running on {machine ? machine.alias : "a machine"}, like a dev server, at a
            temporary address. It ends when you stop it.
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent className="flex-row justify-center">
          <ShareFileButton machine={machine} />
          <NewPreviewButton machine={machine} />
        </EmptyContent>
        {dropped}
      </Empty>
    )
  }

  return (
    <section className="flex flex-col gap-3">
      {dropped}
      <div className="flex items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search previews" />
        <ViewToggle
          value={view}
          onChange={setView}
          views={[
            { value: "live", label: "Live", count: counts.data?.live },
            { value: "ended", label: "Ended", count: counts.data?.ended },
          ]}
        />
        <div className="ml-auto flex gap-2">
          <ShareFileButton machine={machine} />
          <NewPreviewButton machine={machine} />
        </div>
      </div>

      <div>
        <table className="w-full table-fixed text-sm">
          <colgroup>
            <col />
            {!scope && <col className="hidden w-[14%] md:table-column" />}
            <col className="hidden w-[18%] lg:table-column" />
            <col className="hidden w-[14%] sm:table-column" />
            {view === "live" && <col className="hidden w-[18%] md:table-column" />}
            <col className="w-12" />
          </colgroup>
          <thead className="border-b border-divider text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Address</th>
              {!scope && <th className="hidden md:table-cell">Machine</th>}
              <th className="hidden lg:table-cell">Forwards to</th>
              <th className="hidden sm:table-cell">{view === "live" ? "Ends" : "Ended"}</th>
              {view === "live" && <th className="hidden md:table-cell">Status</th>}
              <th className="w-12" aria-label="Actions" />
            </tr>
          </thead>
          <tbody className="divide-y divide-divider border-b border-divider">
            {query.isPending
              ? Array.from({ length: 3 }, (_, index) => (
                  <tr key={index} className="[&>td]:h-14 [&>td]:px-3">
                    <td colSpan={columns}>
                      <Skeleton className="h-5 w-1/3" />
                    </td>
                  </tr>
                ))
              : shown.map((preview) => (
                  <Row
                    key={preview.id}
                    preview={preview}
                    now={now}
                    machineName={scope ? undefined : (aliasOf(preview.owner_machine_id) ?? "—")}
                  />
                ))}
          </tbody>
        </table>
        {query.data && <ListPager page={query.data.page} size={cursorPageSize} total={query.data.total} go={pages.go} />}
        {query.isError && (
          <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
            Couldn’t load previews.
            <Button variant="outline" size="sm" onClick={() => query.refetch()}>
              Retry
            </Button>
          </div>
        )}
        {query.isSuccess && shown.length === 0 && (
          <p className="py-12 text-center text-sm text-muted-foreground">
            {q ? `No previews match “${q}”` : view === "live" ? "No live previews" : "No ended previews"}
          </p>
        )}
      </div>
    </section>
  )
}

function Row({ preview, now, machineName }: { preview: Preview; now: number; machineName?: string }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const [stopping, setStopping] = useState(false)
  const live = !ended(preview, now)
  const host = hostOf(preview.endpoint)

  const entries: Entry[] = live
    ? [
        {
          kind: "item",
          label: "Copy address",
          icon: CopyIcon,
          onSelect: () => void copyWithToast(preview.endpoint, "Address"),
        },
        {
          kind: "item",
          label: "Inspect",
          icon: MagnifyingGlassIcon,
          href: `/previews/${encodeURIComponent(preview.id)}/inspect`,
        },
        { kind: "separator" },
        { kind: "item", label: "Stop preview", icon: StopIcon, destructive: true, onSelect: () => setStopping(true) },
      ]
    : []

  const row = (actions: React.ReactNode) => (
    <tr
      className={`group relative [&>td]:h-14 [&>td]:px-3 ${live ? "transition-colors hover:bg-surface data-popup-open:bg-surface" : ""}`}
    >
      <td>
        <div className="flex min-w-0 items-center gap-2">
          {live ? (
            <a
              href={preview.endpoint}
              target="_blank"
              rel="noopener"
              className="truncate-x font-mono text-code font-medium trim-cap after:absolute after:inset-0"
            >
              {host}
            </a>
          ) : (
            // An ended preview's address no longer answers; the server sends none for a lease that ended.
            <span className="truncate-x font-mono text-code text-muted-foreground trim-cap">{host || "Ended"}</span>
          )}
          {preview.access_mode === "private" && (
            <Badge variant="outline" className="font-normal text-muted-foreground trim-cap">
              Only you
            </Badge>
          )}
        </div>
      </td>
      {machineName !== undefined && <td className="hidden truncate md:table-cell">{machineName}</td>}
      <td className="hidden truncate font-mono text-code text-muted-foreground lg:table-cell">
        {originLabel(preview.target)}
      </td>
      <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
        {live ? endsLabel(preview, now) : formatRelative(endedAt(preview), now)}
      </td>
      {live && (
        <td className="hidden whitespace-nowrap text-muted-foreground md:table-cell">
          <PreviewStatus preview={preview} />
        </td>
      )}
      <td className="text-right">{actions}</td>
    </tr>
  )

  if (!entries.length) return row(null)
  return (
    <RowMenus
      label={`Actions for ${host}`}
      entries={entries}
      row={row}
      dialogs={
        <ConfirmAction
          open={stopping}
          onOpenChange={setStopping}
          title="Stop this preview?"
          description={`${host} stops working right away. The app on the machine keeps running.`}
          action="Stop preview"
          pending="Stopping…"
          mutation={{
            mutationFn: () => stopPreview(preview.id),
            onSuccess: async () => {
              await queryClient.invalidateQueries({ queryKey: tunnelKeys.previews(workspace?.id ?? "") })
              toast.success("Preview stopped")
            },
          }}
        />
      }
    />
  )
}
