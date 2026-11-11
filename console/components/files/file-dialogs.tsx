"use client"

import { CheckCircleIcon, FileIcon, UploadSimpleIcon, WarningCircleIcon, XIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { useMachines, useWorkspace, type Machine } from "@/lib/api"
import { uploadToInbox } from "@/lib/inbox-upload"
import { fileKeys, sendToMachine, shareInboxFile, type InboxFile } from "@/lib/files"
import { formatBytes } from "@/lib/format"
import { opensInBrowser } from "@/lib/tunnels"
import { cn } from "@/lib/utils"
import { Starting } from "@/components/tunnels/new-preview"
import { Choice } from "@/components/tunnels/share-file"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

/** A machine can take files while it's online and its Paperboat service receives them. */
export function canReceive(machine: Machine) {
  const receive = machine.capabilities?.file_receive
  return machine.online && Boolean(receive?.configured && receive.observed)
}

type Upload = { file: File; progress: number; state: "waiting" | "sending" | "sent" | "failed"; error?: string }
type Run = { machine: Pick<Machine, "id" | "alias">; uploads: Upload[] }

export function SendFilesButton({ machine }: { machine?: Machine }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button size="lg" onClick={() => setOpen(true)}>
        <UploadSimpleIcon data-icon="inline-start" />
        Send files
      </Button>
      {open && <SendFilesDialog machine={machine} onClose={() => setOpen(false)} />}
    </>
  )
}

function UploadRow({ entry }: { entry: Upload }) {
  return (
    <li className="flex min-w-0 flex-col gap-1.5 rounded-lg bg-surface p-3 text-sm">
      <div className="flex min-w-0 items-center gap-3">
        <FileIcon className="size-4 shrink-0 text-muted-foreground" />
        <span className="min-w-0 flex-1 truncate">{entry.file.name}</span>
        <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
          {entry.state === "sent" ? (
            <CheckCircleIcon weight="fill" className="size-4 text-success" aria-label="Sent" />
          ) : entry.state === "failed" ? (
            <WarningCircleIcon weight="fill" className="size-4 text-destructive" aria-label="Failed" />
          ) : (
            formatBytes(entry.file.size)
          )}
        </span>
      </div>
      {entry.state === "sending" && (
        <div className="h-1 overflow-hidden rounded-full bg-muted">
          <div className="h-full bg-primary transition-[width]" style={{ width: `${Math.round(entry.progress * 100)}%` }} />
        </div>
      )}
      {entry.error && <p className="text-xs text-destructive">{entry.error}</p>}
    </li>
  )
}

/** The chosen machines as removable chips (the last one stays), then the machines still to add. */
function MachineChips({
  selected,
  rest,
  pending,
  onAdd,
  onRemove,
}: {
  selected: Machine[]
  rest: Machine[]
  pending: boolean
  onAdd: (id: string) => void
  onRemove: (id: string) => void
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground">
      <span>To</span>
      {selected.map((entry) => (
        <Badge key={entry.id} variant="outline" className="h-6 text-xs font-normal text-foreground">
          {entry.alias}
          {selected.length > 1 && (
            <button
              type="button"
              aria-label={`Remove ${entry.alias}`}
              onClick={() => onRemove(entry.id)}
              className="-mr-1 rounded-full text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
            >
              <XIcon />
            </button>
          )}
        </Badge>
      ))}
      {!selected.length && <span className="font-medium text-foreground">{pending ? "…" : "no machine"}</span>}
      {rest.length > 0 && (
        <Choice
          label="Add machine"
          value=""
          min={1}
          options={rest.map((entry) => ({ value: entry.id, label: entry.alias }))}
          onChange={onAdd}
        />
      )}
    </div>
  )
}

/** Sent files and failures across the runs, in the status line's words. */
function statusLine(runs: Run[], busy: boolean) {
  if (runs.length === 1) {
    const { alias } = runs[0].machine
    const sent = runs[0].uploads.filter((entry) => entry.state === "sent").length
    return busy ? `Sending to ${alias}…` : `Sent ${sent} of ${runs[0].uploads.length} to ${alias}’s Inbox.`
  }
  if (busy) return `Sending to ${runs.length} machines…`
  const failed = runs.filter((run) => run.uploads.some((entry) => entry.state === "failed")).length
  return failed
    ? `Sent to ${runs.length - failed} of ${runs.length} machines. ${failed} failed.`
    : `Sent ${runs[0].uploads.length} ${runs[0].uploads.length === 1 ? "file" : "files"} to ${runs.length} machines’ Inboxes.`
}

/**
 * Drop files, they go to one or more machines' Inboxes. The destinations are one line above the
 * drop zone ("To homelab ✕ build ✕ Add machine ▾"), so dropping is the only step; each file shows
 * its own progress.
 */
export function SendFilesDialog({
  machine,
  files: initialFiles,
  onClose,
}: {
  machine?: Machine
  files?: File[]
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const machines = useMachines()
  const workspace = useWorkspace()
  const usable = (machines.data ?? []).filter(canReceive)
  // Until the person changes it, the destination is the page's machine, else the first usable one.
  const [picked, setPicked] = useState<string[]>()
  const fallback = usable.find((entry) => entry.id === machine?.id) ?? usable[0]
  const selected = picked ? usable.filter((entry) => picked.includes(entry.id)) : fallback ? [fallback] : []
  const ids = selected.map((entry) => entry.id)
  const [runs, setRuns] = useState<Run[]>([])
  const cancels = useRef<(() => void)[]>([])
  const inputRef = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)

  const update = (run: number, index: number, change: Partial<Upload>) =>
    setRuns((current) =>
      current.map((entry, at) =>
        at === run
          ? { ...entry, uploads: entry.uploads.map((upload, i) => (i === index ? { ...upload, ...change } : upload)) }
          : entry,
      ),
    )

  const sendTo = async (run: number, target: Machine, files: File[]) => {
    // One connection per machine, which takes the files one after another (a terminal upload's batch).
    // Bytes sent so far are shared out over the files in order for the per-file bars.
    const total = files.reduce((sum, file) => sum + file.size, 0)
    const upload = uploadToInbox(target, workspace?.id ?? "personal", files, ({ phase, transferredBytes }) => {
      let before = 0
      files.forEach((file, index) => {
        const progress = phase === "uploading" && file.size ? Math.min(1, Math.max(0, (transferredBytes - before) / file.size)) : 0
        before += file.size
        update(run, index, { state: "sending", progress: total ? progress : 0 })
      })
    })
    cancels.current.push(upload.cancel)
    files.forEach((_, index) => update(run, index, { state: "sending" }))
    try {
      await upload.done
      files.forEach((_, index) => update(run, index, { state: "sent", progress: 1 }))
    } catch (error) {
      // A refused batch is refused whole; every file of it keeps the machine's reason.
      files.forEach((_, index) => update(run, index, { state: "failed", error: (error as Error).message }))
    }
  }

  const start = async (files: File[]) => {
    if (!selected.length || !files.length) return
    const targets = selected
    setRuns(targets.map((entry) => ({ machine: entry, uploads: files.map((file) => ({ file, progress: 0, state: "waiting" })) })))
    // Machines run in parallel; one failing never stops the others (sendTo catches its own errors).
    await Promise.all(targets.map((entry, run) => sendTo(run, entry, files)))
    await queryClient.invalidateQueries({ queryKey: fileKeys.inboxes })
  }

  // Files dropped on the page start as soon as there's a machine to take them.
  const started = useRef(false)
  useEffect(() => {
    if (!initialFiles?.length || started.current || !selected.length) return
    started.current = true
    void start(initialFiles)
  })

  const busy = runs.some((run) => run.uploads.some((entry) => entry.state === "waiting" || entry.state === "sending"))
  const several = runs.length > 1
  const plural = selected.length > 1

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (next) return
        cancels.current.forEach((cancel) => cancel())
        onClose()
      }}
    >
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Send files</DialogTitle>
        </DialogHeader>
        {runs.length ? (
          <div className="flex min-w-0 flex-col gap-3">
            <div className="flex max-h-72 flex-col gap-3 overflow-y-auto">
              {runs.map((run) => (
                <section key={run.machine.id} className="flex min-w-0 flex-col gap-2">
                  {several && (
                    <h3 className="flex items-baseline justify-between gap-3 text-xs">
                      <span className="min-w-0 truncate font-medium">{run.machine.alias}</span>
                      <span className="shrink-0 text-muted-foreground tabular-nums">
                        {run.uploads.filter((entry) => entry.state === "sent").length} of {run.uploads.length} sent
                      </span>
                    </h3>
                  )}
                  <ul className="flex flex-col gap-2">
                    {run.uploads.map((entry, index) => (
                      <UploadRow key={index} entry={entry} />
                    ))}
                  </ul>
                </section>
              ))}
            </div>
            <p className="text-xs text-muted-foreground" role="status">
              {statusLine(runs, busy)}
            </p>
            {!busy && (
              <DialogFooter>
                <Button size="lg" onClick={onClose}>
                  Done
                </Button>
              </DialogFooter>
            )}
          </div>
        ) : (
          <>
            <MachineChips
              selected={selected}
              rest={usable.filter((entry) => !ids.includes(entry.id))}
              pending={machines.isPending}
              onAdd={(id) => setPicked([...ids, id])}
              onRemove={(id) => setPicked(ids.filter((entry) => entry !== id))}
            />
            <button
              type="button"
              data-file-drop
              disabled={!selected.length}
              onClick={() => inputRef.current?.click()}
              onDragOver={(event) => {
                event.preventDefault()
                event.dataTransfer.dropEffect = "copy"
                setOver(true)
              }}
              onDragLeave={() => setOver(false)}
              onDrop={(event) => {
                event.preventDefault()
                setOver(false)
                void start(Array.from(event.dataTransfer.files))
              }}
              className={cn(
                "flex h-44 flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-input bg-surface text-sm transition-colors outline-none hover:border-ring/60 focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none disabled:opacity-50",
                over && "border-ring bg-ring/5",
              )}
            >
              <UploadSimpleIcon className="size-5 text-muted-foreground" />
              <span className="font-medium">{over ? "Drop to send" : "Drop files here"}</span>
              <span className="text-xs text-muted-foreground">or click to choose</span>
            </button>
            <p className="text-xs text-muted-foreground">
              {selected.length
                ? `Files land in ${plural ? "each machine’s" : `${selected[0].alias}’s`} Paperboat Inbox, in its Downloads folder.`
                : machines.isPending
                  ? "Files land in the machine’s Paperboat Inbox, in its Downloads folder."
                  : usable.length
                    ? "Choose a machine to send to"
                    : "No machine can receive files right now. Bring a machine online to send."}
            </p>
            <input
              ref={inputRef}
              type="file"
              multiple
              hidden
              onChange={(event) => {
                const chosenFiles = Array.from(event.currentTarget.files ?? [])
                event.currentTarget.value = ""
                void start(chosenFiles)
              }}
            />
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

const lifetimes = [
  { hours: 1, label: "1 hour" },
  { hours: 8, label: "8 hours" },
  { hours: 24, label: "24 hours" },
]
const audiences = { public: "Anyone with the link", private: "Only you" } as const

/** "Share link…" on an Inbox file: the machine serves it at a preview link, nothing re-uploaded. */
export function ShareInboxFileDialog({
  machine,
  file,
  onClose,
}: {
  machine: Pick<Machine, "id" | "alias">
  file: InboxFile
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [access, setAccess] = useState<keyof typeof audiences>("public")
  const [hours, setHours] = useState(24)
  const share = useMutation({
    mutationFn: () => shareInboxFile(machine.id, file.id, { access, hours }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["previews"] }),
  })
  const opens = opensInBrowser({ name: file.name, type: "" })
  return (
    <Dialog open onOpenChange={(next) => !next && !share.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Share {file.name}</DialogTitle>
        </DialogHeader>
        {share.data ? (
          <Starting
            id={share.data}
            machineName={machine.alias}
            label={file.name}
            note={opens ? "Opens in the browser." : "Downloads when opened."}
            onDone={onClose}
          />
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-sm text-muted-foreground">
              <span>From {machine.alias} ·</span>
              <Choice
                label={audiences[access]}
                value={access}
                options={Object.entries(audiences).map(([value, label]) => ({ value, label }))}
                onChange={(value) => setAccess(value as keyof typeof audiences)}
              />
              <span aria-hidden>·</span>
              <Choice
                label={lifetimes.find((entry) => entry.hours === hours)!.label}
                value={String(hours)}
                options={lifetimes.map((entry) => ({ value: String(entry.hours), label: entry.label }))}
                onChange={(value) => setHours(Number(value))}
              />
            </div>
            <p className="text-xs text-muted-foreground">
              {opens ? "It opens in the browser." : "It downloads when opened."} {machine.alias} serves it while the
              link lasts.
            </p>
            {share.isError && <p className="text-sm text-destructive">{share.error.message}</p>}
            <DialogFooter>
              <Button variant="secondary" size="lg" onClick={onClose} disabled={share.isPending}>
                Cancel
              </Button>
              <Button size="lg" onClick={() => share.mutate()} disabled={share.isPending}>
                {share.isPending ? "Creating…" : "Create link"}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

/** "Send to…" on an Inbox file: copies it to another machine's Inbox, machine to machine. */
export function SendToMachineDialog({
  machine,
  file,
  onClose,
}: {
  machine: Pick<Machine, "id" | "alias">
  file: InboxFile
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const machines = useMachines()
  const others = (machines.data ?? []).filter((entry) => entry.id !== machine.id && canReceive(entry))
  const [chosen, setChosen] = useState<string>()
  const target = others.find((entry) => entry.id === chosen) ?? others[0]
  const send = useMutation({
    mutationFn: () => sendToMachine(machine.id, file.id, target!.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: fileKeys.inboxes })
      toast.success(`${file.name} sent to ${target!.alias}`)
      onClose()
    },
  })
  return (
    <Dialog open onOpenChange={(next) => !next && !send.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Send {file.name}</DialogTitle>
        </DialogHeader>
        <div className="flex flex-wrap items-center gap-x-1.5 text-sm text-muted-foreground">
          <span>From {machine.alias} to</span>
          <Choice
            label={target?.alias ?? (machines.isPending ? "…" : "no machine")}
            value={target?.id ?? ""}
            options={others.map((entry) => ({ value: entry.id, label: entry.alias }))}
            onChange={setChosen}
          />
        </div>
        <p className="text-xs text-muted-foreground">
          {target
            ? `Sends directly from ${machine.alias} to ${target.alias} (${formatBytes(file.size)}).`
            : "None of your other machines can take files right now."}
        </p>
        {send.isError && <p className="text-sm text-destructive">{send.error.message}</p>}
        <DialogFooter>
          <Button variant="secondary" size="lg" onClick={onClose} disabled={send.isPending}>
            Cancel
          </Button>
          <Button size="lg" onClick={() => send.mutate()} disabled={!target || send.isPending}>
            {send.isPending ? "Sending…" : "Send"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
