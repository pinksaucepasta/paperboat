"use client"

import { CheckCircleIcon, FileIcon, UploadSimpleIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { useMachines, type Machine } from "@/lib/api"
import { fileKeys, sendToInbox, sendToMachine, shareInboxFile, type InboxFile } from "@/lib/files"
import { formatBytes } from "@/lib/format"
import { opensInBrowser } from "@/lib/tunnels"
import { cn } from "@/lib/utils"
import { Starting } from "@/components/tunnels/new-preview"
import { Choice } from "@/components/tunnels/share-file"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

/** A machine can take files while it's online and its Paperboat service receives them. */
export function canReceive(machine: Machine) {
  const receive = machine.capabilities?.file_receive
  return machine.online && Boolean(receive?.configured && receive.observed)
}

type Upload = { file: File; progress: number; state: "waiting" | "sending" | "sent" | "failed"; error?: string }

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

/**
 * Drop files, they go to a machine's Inbox. The destination is one line above the drop zone
 * ("To homelab ▾"), so dropping is the only step; each file shows its own progress.
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
  const usable = (machines.data ?? []).filter(canReceive)
  const [chosen, setChosen] = useState<string | undefined>(machine?.id)
  const target = usable.find((entry) => entry.id === chosen) ?? usable[0]
  const [uploads, setUploads] = useState<Upload[]>([])
  const cancels = useRef<(() => void)[]>([])
  const inputRef = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)

  const update = (index: number, change: Partial<Upload>) =>
    setUploads((current) => current.map((entry, at) => (at === index ? { ...entry, ...change } : entry)))

  const start = async (files: File[]) => {
    if (!target || !files.length) return
    setUploads(files.map((file) => ({ file, progress: 0, state: "waiting" })))
    // One at a time: the machine writes each file in full before the next starts.
    for (const [index, file] of files.entries()) {
      update(index, { state: "sending" })
      const upload = sendToInbox(target.id, file, (progress) => update(index, { progress }))
      cancels.current.push(upload.cancel)
      try {
        await upload.done
        update(index, { state: "sent", progress: 1 })
      } catch (error) {
        update(index, { state: "failed", error: (error as Error).message })
      }
    }
    await queryClient.invalidateQueries({ queryKey: fileKeys.inbox(target.id) })
  }

  // Files dropped on the page start as soon as there's a machine to take them.
  const started = useRef(false)
  useEffect(() => {
    if (!initialFiles?.length || started.current || !target) return
    started.current = true
    void start(initialFiles)
  })

  const busy = uploads.some((entry) => entry.state === "waiting" || entry.state === "sending")
  const sent = uploads.filter((entry) => entry.state === "sent").length

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
        {uploads.length ? (
          <div className="flex min-w-0 flex-col gap-3">
            <ul className="flex max-h-72 flex-col gap-2 overflow-y-auto">
              {uploads.map((entry, index) => (
                <li key={index} className="flex min-w-0 flex-col gap-1.5 rounded-lg bg-surface p-3 text-sm">
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
                      <div
                        className="h-full bg-primary transition-[width]"
                        style={{ width: `${Math.round(entry.progress * 100)}%` }}
                      />
                    </div>
                  )}
                  {entry.error && <p className="text-xs text-destructive">{entry.error}</p>}
                </li>
              ))}
            </ul>
            <p className="text-xs text-muted-foreground" role="status">
              {busy
                ? `Sending to ${target?.alias ?? "the machine"}…`
                : `Sent ${sent} of ${uploads.length} to ${target?.alias ?? "the machine"}’s Inbox.`}
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
            <div className="flex flex-wrap items-center gap-x-1.5 text-sm text-muted-foreground">
              <span>To</span>
              <Choice
                label={target?.alias ?? (machines.isPending ? "…" : "no machine")}
                value={target?.id ?? ""}
                options={usable.map((entry) => ({ value: entry.id, label: entry.alias }))}
                onChange={setChosen}
              />
            </div>
            <button
              type="button"
              data-file-drop
              disabled={!target}
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
              {target || machines.isPending
                ? "They land in its Paperboat Inbox, in the machine’s Downloads folder."
                : "None of your machines can take files right now. Bring one online to send."}
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
  machine: Machine
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
  machine: Machine
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
      await queryClient.invalidateQueries({ queryKey: fileKeys.inbox(target!.id) })
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
            ? `It goes straight from ${machine.alias} to ${target.alias}, ${formatBytes(file.size)}.`
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
