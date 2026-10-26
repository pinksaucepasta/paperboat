"use client"

import { DownloadSimpleIcon, FolderIcon, LinkIcon, PaperPlaneTiltIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { ApiError, meQuery, useApiQuery, useMachine, useMachines, useWorkspace, type Machine } from "@/lib/api"
import {
  decideRequest,
  deleteInboxFile,
  downloadUrl,
  fileKeys,
  inboxLive,
  inboxQuery,
  requestsQuery,
  type InboxFile,
  type InboxRequest,
} from "@/lib/files"
import { formatBytes } from "@/lib/format"
import {
  SendFilesButton,
  SendFilesDialog,
  SendToMachineDialog,
  ShareInboxFileDialog,
} from "@/components/files/file-dialogs"
import { DropOverlay, usePageFileDrop } from "@/components/file-drop"
import { Transfers } from "@/components/files/transfers"
import { ConfirmAction, SearchField, useListScope } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

/** The machine page's Files tab. */
export function MachineFiles() {
  return <FileList machine={useCurrentMachine()} />
}

type Row = { file: InboxFile; machine: { id: string; alias: string } }
const rowKey = (row: Row) => `${row.machine.id}/${row.file.id}`

/**
 * One machine's Paperboat Inbox: files sent from here, dropped into a terminal, sent with `pb send`,
 * or approved from a teammate. Files are browsed one machine at a time (pick it above); the machine
 * answers for its own Inbox live, nothing is stored, and an offline machine's Inbox is unavailable
 * until it is back. Downloading and deleting wait for `inboxLive`.
 */
export function FileList({ machine: tabMachine }: { machine?: Machine }) {
  const { scope } = useListScope(tabMachine)
  return (
    <section className="flex flex-col gap-4">
      <Requests scope={scope} />
      <Transfers scope={scope} />
      {scope ? <MachineInbox machineId={scope} /> : <ChooseMachine />}
    </section>
  )
}

function ChooseMachine() {
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <FolderIcon />
        </EmptyMedia>
        <EmptyTitle>Choose a machine</EmptyTitle>
        <EmptyDescription>Each machine keeps its own Inbox. Pick one above to see its files.</EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}

function MachineInbox({ machineId }: { machineId: string }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const { data: machine } = useMachine(machineId)
  const inbox = useApiQuery({
    ...inboxQuery(machineId, workspace?.id ?? ""),
    enabled: Boolean(workspace && machine?.online),
  })
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim().toLowerCase())
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [deletingSelected, setDeletingSelected] = useState(false)
  const drop = usePageFileDrop()

  const offline = (machine && !machine.online) || (inbox.error instanceof ApiError && inbox.error.code === "machine_offline")
  const outdated = inbox.error instanceof ApiError && inbox.error.code === "machine_update_required"
  const loading = !machine || (machine.online && inbox.isPending && !offline)
  const owner = { id: machineId, alias: machine?.alias ?? "" }
  const rows: Row[] = (inbox.data?.items ?? [])
    .filter((file) => !q || file.name.toLowerCase().includes(q))
    .map((file) => ({ file, machine: owner }))
  const truncated = inbox.data && inbox.data.total > inbox.data.items.length
  const selectedRows = rows.filter((row) => selected.has(rowKey(row)))
  const allSelected = rows.length > 0 && selectedRows.length === rows.length
  const toggle = (key: string, on: boolean) =>
    setSelected((current) => {
      const next = new Set(current)
      if (on) next.add(key)
      else next.delete(key)
      return next
    })
  const columns = 4

  const dropped = (
    <>
      {drop.over && !drop.files.length && <DropOverlay title="Drop to send" detail="They go to a machine’s Inbox" />}
      {drop.files.length > 0 && <SendFilesDialog machine={machine} files={drop.files} onClose={drop.clear} />}
    </>
  )

  const empty = !loading && rows.length === 0 && !q && !inbox.isError

  if (offline) {
    return (
      <Empty className="py-16" data-inbox-offline>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <FolderIcon />
          </EmptyMedia>
          <EmptyTitle>{machine?.alias ?? "This machine"}: machine offline, Inbox unavailable</EmptyTitle>
          <EmptyDescription>An Inbox is read from its machine, so it shows when the machine is back online.</EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  }

  return (
    <>
      {dropped}
      {empty ? (
        <Empty className="py-16">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <FolderIcon />
            </EmptyMedia>
            <EmptyTitle>{machine ? `${machine.alias}’s Inbox is empty` : "The Inbox is empty"}</EmptyTitle>
            <EmptyDescription>
              Files you send here, drop into a terminal, or send with pb send land in the machine’s Paperboat Inbox.
            </EmptyDescription>
          </EmptyHeader>
          {inboxLive.upload && (
            <EmptyContent>
              <SendFilesButton machine={machine} />
            </EmptyContent>
          )}
        </Empty>
      ) : (
        <div className="flex flex-col gap-3">
          <div className="flex items-center gap-2">
            <SearchField value={search} onChange={setSearch} placeholder="Search files" />
            <div className="ml-auto flex items-center gap-2">
              {selectedRows.length > 0 && (
                <Button
                  variant="secondary"
                  size="lg"
                  className="text-destructive"
                  onClick={() => setDeletingSelected(true)}
                >
                  <TrashIcon data-icon="inline-start" />
                  Delete {selectedRows.length}
                </Button>
              )}
              {inboxLive.upload && <SendFilesButton machine={machine} />}
            </div>
          </div>

          <table className="w-full table-fixed text-sm">
            <colgroup>
              <col className="w-10" />
              <col />
              <col className="hidden w-[10%] sm:table-column" />
              <col className="hidden w-[16%] sm:table-column" />
              <col className="w-12" />
            </colgroup>
            <thead className="border-b border-divider text-left text-xs text-muted-foreground">
              <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
                <th>
                  <Checkbox
                    aria-label="Select all"
                    checked={allSelected}
                    disabled={!rows.length}
                    onCheckedChange={(on) => setSelected(on ? new Set(rows.map(rowKey)) : new Set())}
                  />
                </th>
                <th>Name</th>
                <th className="hidden text-right sm:table-cell">Size</th>
                <th className="hidden sm:table-cell">Received</th>
                <th className="w-12" aria-label="Actions" />
              </tr>
            </thead>
            <tbody className="divide-y divide-divider border-b border-divider">
              {loading && !rows.length
                ? Array.from({ length: 3 }, (_, index) => (
                    <tr key={index} className="[&>td]:h-14 [&>td]:px-3">
                      <td colSpan={columns + 1}>
                        <Skeleton className="h-5 w-1/3" />
                      </td>
                    </tr>
                  ))
                : rows.map((row) => (
                    <FileRow
                      key={rowKey(row)}
                      row={row}
                      selected={selected.has(rowKey(row))}
                      onSelect={(on) => toggle(rowKey(row), on)}
                    />
                  ))}
            </tbody>
          </table>
          {!loading && rows.length === 0 && q && (
            <p className="py-12 text-center text-sm text-muted-foreground">No files match “{search.trim()}”</p>
          )}
          {inbox.isError && !outdated && (
            <p className="text-sm text-muted-foreground">
              Couldn’t read {owner.alias}’s Inbox. {inbox.error.message}{" "}
              <button
                type="button"
                className="underline-offset-4 hover:text-foreground hover:underline"
                onClick={() => void inbox.refetch()}
              >
                Try again
              </button>
            </p>
          )}
          {outdated && <p className="text-sm text-muted-foreground">Update Paperboat on {owner.alias} to browse its Inbox here.</p>}
          {truncated && <p className="text-sm text-muted-foreground">Showing the newest 200 files of {owner.alias}’s Inbox.</p>}
        </div>
      )}

      <ConfirmAction
        open={deletingSelected}
        onOpenChange={setDeletingSelected}
        title={`Delete ${selectedRows.length} ${selectedRows.length === 1 ? "file" : "files"}?`}
        description={`They’re removed from ${owner.alias}’s Inbox. This can’t be undone.`}
        action="Delete"
        pending="Deleting…"
        mutation={{
          mutationFn: async () => {
            await Promise.all(selectedRows.map((row) => deleteInboxFile(row.machine.id, row.file.id)))
          },
          onSuccess: async () => {
            setSelected(new Set())
            await queryClient.invalidateQueries({ queryKey: fileKeys.inboxes })
          },
        }}
      />
    </>
  )
}


function FileRow({
  row,
  selected,
  onSelect,
}: {
  row: Row
  selected: boolean
  onSelect: (on: boolean) => void
}) {
  const queryClient = useQueryClient()
  const { file, machine } = row
  const [dialog, setDialog] = useState<"share" | "send" | "delete" | null>(null)
  const href = downloadUrl(machine.id, file.id)

  // Each action is answered live by the machine; it appears when its endpoint is deployed.
  const entries: Entry[] = [
    ...(inboxLive.share
      ? ([{ kind: "item", label: "Share link…", icon: LinkIcon, onSelect: () => setDialog("share") }] satisfies Entry[])
      : []),
    ...(inboxLive.send
      ? ([
          { kind: "item", label: "Send to…", icon: PaperPlaneTiltIcon, onSelect: () => setDialog("send") },
        ] satisfies Entry[])
      : []),
    ...((inboxLive.share || inboxLive.send) && inboxLive.remove ? ([{ kind: "separator" }] satisfies Entry[]) : []),
    ...(inboxLive.remove
      ? ([
          { kind: "item", label: "Delete", icon: TrashIcon, destructive: true, onSelect: () => setDialog("delete") },
        ] satisfies Entry[])
      : []),
  ]
  const rowOf = (actions: React.ReactNode) => (
    <tr
      data-selected={selected || undefined}
      className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface data-selected:bg-surface [&>td]:h-14 [&>td]:px-3"
    >
      {/* Above the row's download link, so ticking a box never starts a download. */}
      <td className="relative z-10">
        {inboxLive.remove && (
          <Checkbox aria-label={`Select ${file.name}`} checked={selected} onCheckedChange={onSelect} />
        )}
      </td>
      <td>
        <div className="flex min-w-0 items-center gap-2">
          {inboxLive.content ? (
            /* Clicking a file downloads it, streamed from the machine. */
            <>
              <a
                href={href}
                download={file.name}
                className="truncate-x font-medium trim-cap after:absolute after:inset-0"
                title={`Download ${file.name}`}
              >
                {file.name}
              </a>
              <DownloadSimpleIcon className="size-3.5 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
            </>
          ) : (
            <span className="truncate-x font-medium trim-cap">{file.name}</span>
          )}
        </div>
      </td>
      <td className="hidden text-right whitespace-nowrap text-muted-foreground tabular-nums sm:table-cell">
        {formatBytes(file.size)}
      </td>
      <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
        <span className="relative z-10">
          <Time iso={file.received_at} relative />
        </span>
      </td>
      <td className="text-right">{actions}</td>
    </tr>
  )
  if (!entries.some((entry) => entry.kind === "item")) return rowOf(null)
  return (
    <RowMenus
      label={`Actions for ${file.name}`}
      entries={entries}
      row={rowOf}
      dialogs={
        <>
          {dialog === "share" && <ShareInboxFileDialog machine={machine} file={file} onClose={() => setDialog(null)} />}
          {dialog === "send" && <SendToMachineDialog machine={machine} file={file} onClose={() => setDialog(null)} />}
          <ConfirmAction
            open={dialog === "delete"}
            onOpenChange={(open) => setDialog(open ? "delete" : null)}
            title={`Delete ${file.name}?`}
            description={`It’s removed from ${machine.alias}’s Inbox. This can’t be undone.`}
            action="Delete"
            pending="Deleting…"
            mutation={{
              mutationFn: () => deleteInboxFile(machine.id, file.id),
              onSuccess: async () => {
                await queryClient.invalidateQueries({ queryKey: fileKeys.inboxes })
                toast.success(`${file.name} deleted`)
              },
            }}
          />
        </>
      }
    />
  )
}

/**
 * Files teammates want to send to your machines (Team Inbox). Shown only while something waits
 * for your answer; approving lets the transfer run, declining tells the sender.
 */
function Requests({ scope }: { scope?: string | null }) {
  const workspace = useWorkspace()
  const { data: me } = useApiQuery(meQuery)
  const machines = useMachines()
  const requests = useApiQuery({ ...requestsQuery(workspace?.id ?? ""), enabled: Boolean(workspace) })
  const waiting = (requests.data ?? []).filter(
    (request) =>
      request.status === "pending" &&
      request.recipient_account === me?.id &&
      (!scope || request.destination_machine_id === scope),
  )
  if (!waiting.length) return null
  const aliasOf = (id: string) => machines.data?.find((entry) => entry.id === id)?.alias ?? "your machine"
  return (
    <div className="flex flex-col gap-2">
      <h2 className="text-sm font-medium">Waiting for you</h2>
      <div className="flex flex-col divide-y divide-divider rounded-lg bg-surface px-4">
        {waiting.map((request) => (
          <RequestRow
            key={request.request_id}
            request={request}
            destination={aliasOf(request.destination_machine_id)}
          />
        ))}
      </div>
    </div>
  )
}

function RequestRow({ request, destination }: { request: InboxRequest; destination: string }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const decide = useMutation({
    mutationFn: (decision: "approve" | "decline") => decideRequest(request, decision),
    onSuccess: async (_, decision) => {
      await queryClient.invalidateQueries({ queryKey: fileKeys.requests(workspace?.id ?? "") })
      toast.success(decision === "approve" ? `Approved; it arrives in ${destination}’s Inbox` : "Declined")
    },
    onError: (error) => toast.error("Couldn’t answer the request", { description: error.message }),
  })
  const [first, ...rest] = request.files
  const total = request.files.reduce((sum, file) => sum + file.size, 0)
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 py-3 text-sm">
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate">
          <span className="font-medium">{first?.basename}</span>
          {rest.length > 0 && ` and ${rest.length} more`}
        </span>
        <span className="text-xs text-muted-foreground">
          A teammate wants to send {formatBytes(total)} to {destination} · expires{" "}
          <Time iso={request.expires_at} relative />
        </span>
      </div>
      <div className="flex gap-2">
        <Button variant="secondary" disabled={decide.isPending} onClick={() => decide.mutate("decline")}>
          Decline
        </Button>
        <Button disabled={decide.isPending} onClick={() => decide.mutate("approve")}>
          Approve
        </Button>
      </div>
    </div>
  )
}
