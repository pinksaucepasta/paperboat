"use client"

import { DownloadSimpleIcon, FolderIcon, LinkIcon, PaperPlaneTiltIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueries, useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { meQuery, useApiQuery, useMachines, useWorkspace, type Machine } from "@/lib/api"
import {
  decideRequest,
  deleteInboxFile,
  downloadUrl,
  fileKeys,
  inboxQuery,
  requestsQuery,
  type InboxFile,
  type InboxRequest,
} from "@/lib/files"
import { formatBytes } from "@/lib/format"
import {
  canReceive,
  SendFilesButton,
  SendFilesDialog,
  SendToMachineDialog,
  ShareInboxFileDialog,
} from "@/components/files/file-dialogs"
import { DropOverlay, usePageFileDrop } from "@/components/file-drop"
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

type Row = { file: InboxFile; machine: Machine }
const rowKey = (row: Row) => `${row.machine.id}/${row.file.id}`

/** Where a file came from, in a word or two. */
function sourceLabel(file: InboxFile, aliasOf: (id: string) => string | undefined) {
  switch (file.source.kind) {
    case "console":
      return "Console"
    case "terminal":
      return "Terminal"
    case "teammate":
      return file.source.name ?? "Teammate"
    case "machine":
      return (file.source.machine_id && aliasOf(file.source.machine_id)) ?? file.source.name ?? "Another machine"
  }
}

/**
 * What's in your machines' Paperboat Inboxes: files sent from here, dropped into a terminal, sent
 * with `pb send`, or approved from a teammate. Each machine answers for its own Inbox while it's
 * online; offline machines are named under the list. Dropping files anywhere sends them.
 */
export function FileList({ machine: tabMachine }: { machine?: Machine }) {
  const queryClient = useQueryClient()
  const { scope, machine, aliasOf } = useListScope(tabMachine)
  const machines = useMachines()
  const inScope = (machines.data ?? []).filter((entry) => !scope || entry.id === scope)
  const readable = inScope.filter(canReceive)
  const offline = inScope.filter((entry) => !canReceive(entry))
  const inboxes = useQueries({ queries: readable.map((entry) => inboxQuery(entry.id)) })
  const [search, setSearch] = useState("")
  const text = useDeferredValue(search.trim().toLowerCase())
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [deletingSelected, setDeletingSelected] = useState(false)
  const drop = usePageFileDrop()

  const loading = machines.isPending || inboxes.some((query) => query.isPending)
  const rows: Row[] = readable
    .flatMap((entry, index) => (inboxes[index]?.data ?? []).map((file) => ({ file, machine: entry })))
    .filter((row) => !text || row.file.name.toLowerCase().includes(text))
    .sort((a, b) => Date.parse(b.file.received_at) - Date.parse(a.file.received_at))
  const failed = readable.filter((_, index) => inboxes[index]?.isError)
  const selectedRows = rows.filter((row) => selected.has(rowKey(row)))
  const allSelected = rows.length > 0 && selectedRows.length === rows.length
  const toggle = (key: string, on: boolean) =>
    setSelected((current) => {
      const next = new Set(current)
      if (on) next.add(key)
      else next.delete(key)
      return next
    })
  const columns = 5 + (scope ? 0 : 1)

  const dropped = (
    <>
      {drop.over && !drop.files.length && <DropOverlay title="Drop to send" detail="They go to a machine’s Inbox" />}
      {drop.files.length > 0 && <SendFilesDialog machine={machine} files={drop.files} onClose={drop.clear} />}
    </>
  )

  const empty = !loading && rows.length === 0 && !text && failed.length === 0

  return (
    <section className="flex flex-col gap-4">
      {dropped}
      <Requests scope={scope} />
      {empty ? (
        <Empty className="py-16">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <FolderIcon />
            </EmptyMedia>
            <EmptyTitle>{machine ? `${machine.alias}’s Inbox is empty` : "Inboxes are empty"}</EmptyTitle>
            <EmptyDescription>
              Files you send here, drop into a terminal, or send with pb send land in the machine’s Paperboat Inbox.
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <SendFilesButton machine={machine} />
          </EmptyContent>
          <OfflineNote machines={offline} />
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
              <SendFilesButton machine={machine} />
            </div>
          </div>

          <table className="w-full table-fixed text-sm">
            <colgroup>
              <col className="w-10" />
              <col />
              {!scope && <col className="hidden w-[16%] md:table-column" />}
              <col className="hidden w-[16%] lg:table-column" />
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
                {!scope && <th className="hidden md:table-cell">Machine</th>}
                <th className="hidden lg:table-cell">From</th>
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
                      showMachine={!scope}
                      from={sourceLabel(row.file, aliasOf)}
                      selected={selected.has(rowKey(row))}
                      onSelect={(on) => toggle(rowKey(row), on)}
                    />
                  ))}
            </tbody>
          </table>
          {!loading && rows.length === 0 && text && (
            <p className="py-12 text-center text-sm text-muted-foreground">No files match “{search.trim()}”</p>
          )}
          {failed.length > 0 && (
            <p className="text-sm text-muted-foreground">
              Couldn’t read {failed.map((entry) => entry.alias).join(", ")}’s Inbox.{" "}
              <button
                type="button"
                className="underline-offset-4 hover:text-foreground hover:underline"
                onClick={() =>
                  failed.forEach((entry) => queryClient.invalidateQueries({ queryKey: fileKeys.inbox(entry.id) }))
                }
              >
                Try again
              </button>
            </p>
          )}
          <OfflineNote machines={offline} />
        </div>
      )}

      <ConfirmAction
        open={deletingSelected}
        onOpenChange={setDeletingSelected}
        title={`Delete ${selectedRows.length} ${selectedRows.length === 1 ? "file" : "files"}?`}
        description="They’re removed from the machines’ Inboxes. This can’t be undone."
        action="Delete"
        pending="Deleting…"
        mutation={{
          mutationFn: async () => {
            await Promise.all(selectedRows.map((row) => deleteInboxFile(row.machine.id, row.file.id)))
          },
          onSuccess: async () => {
            const affected = new Set(selectedRows.map((row) => row.machine.id))
            setSelected(new Set())
            await Promise.all(
              [...affected].map((id) => queryClient.invalidateQueries({ queryKey: fileKeys.inbox(id) })),
            )
          },
        }}
      />
    </section>
  )
}

/** Machines whose Inbox can't be read right now; their files show when they're back. */
function OfflineNote({ machines }: { machines: Machine[] }) {
  if (!machines.length) return null
  const names = machines.map((entry) => entry.alias)
  const list = names.length > 3 ? `${names.slice(0, 3).join(", ")} and ${names.length - 3} more` : names.join(", ")
  return (
    <p className="text-sm text-muted-foreground">
      {list} {names.length === 1 ? "is" : "are"} offline.{" "}
      {names.length === 1 ? "Its Inbox shows" : "Their Inboxes show"} when {names.length === 1 ? "it’s" : "they’re"}{" "}
      back.
    </p>
  )
}

function FileRow({
  row,
  showMachine,
  from,
  selected,
  onSelect,
}: {
  row: Row
  showMachine: boolean
  from: string
  selected: boolean
  onSelect: (on: boolean) => void
}) {
  const queryClient = useQueryClient()
  const { file, machine } = row
  const [dialog, setDialog] = useState<"share" | "send" | "delete" | null>(null)
  const href = downloadUrl(machine.id, file.id)

  const entries: Entry[] = [
    { kind: "item", label: "Share link…", icon: LinkIcon, onSelect: () => setDialog("share") },
    { kind: "item", label: "Send to…", icon: PaperPlaneTiltIcon, onSelect: () => setDialog("send") },
    { kind: "separator" },
    { kind: "item", label: "Delete", icon: TrashIcon, destructive: true, onSelect: () => setDialog("delete") },
  ]

  return (
    <RowMenus
      label={`Actions for ${file.name}`}
      entries={entries}
      row={(actions) => (
        <tr
          data-selected={selected || undefined}
          className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface data-selected:bg-surface [&>td]:h-14 [&>td]:px-3"
        >
          {/* Above the row's download link, so ticking a box never starts a download. */}
          <td className="relative z-10">
            <Checkbox aria-label={`Select ${file.name}`} checked={selected} onCheckedChange={onSelect} />
          </td>
          <td>
            <div className="flex min-w-0 items-center gap-2">
              {/* Clicking a file downloads it, streamed from the machine. */}
              <a
                href={href}
                download={file.name}
                className="truncate-x font-medium trim-cap after:absolute after:inset-0"
                title={`Download ${file.name}`}
              >
                {file.name}
              </a>
              <DownloadSimpleIcon className="size-3.5 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
            </div>
          </td>
          {showMachine && <td className="hidden truncate md:table-cell">{machine.alias}</td>}
          <td className="hidden truncate text-muted-foreground lg:table-cell">{from}</td>
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
      )}
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
                await queryClient.invalidateQueries({ queryKey: fileKeys.inbox(machine.id) })
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
