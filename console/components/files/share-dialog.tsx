"use client"

import { FileIcon, UploadSimpleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import { useMachines, useWorkspace, type Machine } from "@/lib/api"
import { inboxLive, shareInboxFile, pickHost, shareNewFile, type InboxFile } from "@/lib/files"
import { formatBytes } from "@/lib/format"
import { opensInBrowser, type Preview } from "@/lib/tunnels"
import { cn } from "@/lib/utils"
import { Choice } from "@/components/choice"
import { canReceive } from "@/components/files/file-dialogs"
import { canHostTerminal } from "@/components/terminal/new-terminal"
import { canPreview } from "@/components/tunnels/machine-choice"
import { Starting } from "@/components/tunnels/new-preview"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

/*
 * Share a file as a link (like Vercel's drop.new), from the Files page: a new file dropped here, or one
 * already in a machine's Inbox ("Share link" in its row menu). Where it's hosted, who can open it and for
 * how long are one line of defaults above the drop zone, so dropping is the only step. The links are
 * previews, so they stay listed on the Previews page.
 */

const lifetimes = [
  { hours: 1, label: "1 hour" },
  { hours: 8, label: "8 hours" },
  { hours: 24, label: "24 hours" },
]
const audiences = { public: "Anyone with the link", private: "Only you" } as const
type Access = Preview["access_mode"]

/**
 * Where a new file is hosted. Machines today; "Team storage" will be one more kind in this list, with its own
 * entry in `host` below (the compiler asks for it) and nothing else changing in the dialog.
 */
type Destination = { kind: "machine"; id: string; label: string }
const destinationValue = (destination: Destination) => `${destination.kind}:${destination.id}`
// A machine hosts a share when it can serve previews, receive files and carry the terminal connection the upload rides.
const destinationsOf = (machines: Machine[]): Destination[] =>
  machines.filter((machine) => canPreview(machine) && canReceive(machine) && canHostTerminal(machine)).map((machine) => ({ kind: "machine", id: machine.id, label: machine.alias }))

type HostInput = { file: File; workspace: string; access: Access; hours: number }
const host: Record<Destination["kind"], (destination: Destination, input: HostInput, onProgress: (fraction: number) => void) => ReturnType<typeof shareNewFile>> = {
  machine: (destination, input, onProgress) => shareNewFile({ ...input, machine: destination }, onProgress),
}

const rememberedKey = "paperboat:share-file-destination"
function rememberedDestination(): string | null {
  try {
    return localStorage.getItem(rememberedKey)
  } catch {
    return null
  }
}
function rememberDestination(value: string) {
  try {
    localStorage.setItem(rememberedKey, value)
  } catch {
    // Blocked storage: the next share starts from the default destination.
  }
}

export type ShareSource =
  /** A new file: dropped or chosen in the dialog, or `file` already in hand. `machine` is the page's machine, if any. */
  | { kind: "upload"; machine?: Machine; file?: File }
  /** A file already in this machine's Inbox. */
  | { kind: "inbox"; machine: Pick<Machine, "id" | "alias">; file: InboxFile }

export function ShareFileButton({ machine }: { machine?: Machine }) {
  const [open, setOpen] = useState(false)
  if (!inboxLive.share) return null
  return (
    <>
      <Button variant="secondary" size="lg" onClick={() => setOpen(true)}>
        <UploadSimpleIcon data-icon="inline-start" />
        Share a file
      </Button>
      {open && <ShareDialog source={{ kind: "upload", machine }} onClose={() => setOpen(false)} />}
    </>
  )
}

type Shared = { id: string; hostName: string; name: string; opens: boolean }

/** The one share dialog: the same defaults line and the same going-live step for a new file and an Inbox file. */
export function ShareDialog({ source, onClose }: { source: ShareSource; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [access, setAccess] = useState<Access>("public")
  const [hours, setHours] = useState(24)
  const [shared, setShared] = useState<Shared>()
  const cancel = useRef<() => void>(undefined)
  const done = (next: Shared) => {
    setShared(next)
    void queryClient.invalidateQueries({ queryKey: ["previews"] })
  }
  const settings = { access, hours, setAccess, setHours }
  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (next) return
        cancel.current?.()
        onClose()
      }}
    >
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{source.kind === "inbox" ? `Share ${source.file.name}` : "Share a file"}</DialogTitle>
        </DialogHeader>
        {shared ? (
          <Starting
            id={shared.id}
            machineName={shared.hostName}
            label={shared.name}
            note={shared.opens ? "Opens in the browser." : "Downloads when opened."}
            onDone={onClose}
          />
        ) : source.kind === "upload" ? (
          <UploadBody source={source} settings={settings} onShared={done} registerCancel={(fn) => (cancel.current = fn)} />
        ) : (
          <InboxBody source={source} settings={settings} onShared={done} onCancel={onClose} />
        )}
      </DialogContent>
    </Dialog>
  )
}

type Settings = { access: Access; hours: number; setAccess: (value: Access) => void; setHours: (value: number) => void }

/** Who can open it and for how long: the two defaults both kinds of share have. */
function SettingsChoices({ settings }: { settings: Settings }) {
  return (
    <>
      <Choice
        label={audiences[settings.access]}
        value={settings.access}
        options={Object.entries(audiences).map(([value, label]) => ({ value, label }))}
        onChange={(value) => settings.setAccess(value as Access)}
      />
      <span aria-hidden>·</span>
      <Choice
        label={lifetimes.find((entry) => entry.hours === settings.hours)!.label}
        value={String(settings.hours)}
        options={lifetimes.map((entry) => ({ value: String(entry.hours), label: entry.label }))}
        onChange={(value) => settings.setHours(Number(value))}
      />
    </>
  )
}

function InboxBody({
  source,
  settings,
  onShared,
  onCancel,
}: {
  source: Extract<ShareSource, { kind: "inbox" }>
  settings: Settings
  onShared: (shared: Shared) => void
  onCancel: () => void
}) {
  const share = useMutation({
    mutationFn: () => shareInboxFile(source.machine.id, source.file.id, { access: settings.access, hours: settings.hours }),
    onSuccess: (id) =>
      onShared({
        id,
        hostName: source.machine.alias,
        name: source.file.name,
        opens: opensInBrowser({ name: source.file.name, type: "" }),
      }),
  })
  const opens = opensInBrowser({ name: source.file.name, type: "" })
  return (
    <>
      <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-sm text-muted-foreground">
        <span>From {source.machine.alias} ·</span>
        <SettingsChoices settings={settings} />
      </div>
      <p className="text-xs text-muted-foreground">
        {opens ? "It opens in the browser." : "It downloads when opened."} {source.machine.alias} serves it while the link
        lasts.
      </p>
      {share.isError && <p className="text-sm text-destructive">{share.error.message}</p>}
      <DialogFooter>
        <Button variant="secondary" size="lg" onClick={onCancel} disabled={share.isPending}>
          Cancel
        </Button>
        <Button size="lg" onClick={() => share.mutate()} disabled={share.isPending}>
          {share.isPending ? "Creating…" : "Create link"}
        </Button>
      </DialogFooter>
    </>
  )
}

function UploadBody({
  source,
  settings,
  onShared,
  registerCancel,
}: {
  source: Extract<ShareSource, { kind: "upload" }>
  settings: Settings
  onShared: (shared: Shared) => void
  registerCancel: (cancel: (() => void) | undefined) => void
}) {
  const workspace = useWorkspace()
  const machines = useMachines()
  const destinations = destinationsOf(machines.data ?? [])
  const [chosen, setChosen] = useState<string | null>(() =>
    source.machine ? `machine:${source.machine.id}` : rememberedDestination(),
  )
  const destination = pickHost(destinations, (entry) => destinationValue(entry) === chosen, Boolean(source.machine))
  const [file, setFile] = useState<File>()
  const [progress, setProgress] = useState(0)
  const [error, setError] = useState<string>()
  const cancel = useRef<() => void>(undefined)
  const inputRef = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)

  const start = (next: File) => {
    if (!destination) return
    setFile(next)
    setError(undefined)
    setProgress(0)
    rememberDestination(destinationValue(destination))
    const upload = host[destination.kind](
      destination,
      { file: next, workspace: workspace?.id ?? "personal", access: settings.access, hours: settings.hours },
      setProgress,
    )
    cancel.current = upload.cancel
    registerCancel(upload.cancel)
    upload.done.then(
      (id) => {
        registerCancel(undefined)
        onShared({ id, hostName: destination.label, name: next.name, opens: opensInBrowser(next) })
      },
      (failure: Error) => {
        registerCancel(undefined)
        setFile(undefined)
        setError(failure.message)
      },
    )
  }

  // A file in hand (dropped on a page) starts as soon as there's somewhere to host it.
  const started = useRef(false)
  useEffect(() => {
    if (!source.file || started.current || !destination) return
    started.current = true
    start(source.file)
  })

  if (file)
    return (
      <div className="flex flex-col gap-3 py-2">
        <div className="flex min-w-0 items-center gap-3 rounded-lg bg-surface p-3 text-sm">
          <FileIcon className="size-5 shrink-0 text-muted-foreground" />
          <span className="min-w-0 flex-1 truncate font-medium">{file.name}</span>
          <span className="shrink-0 text-xs text-muted-foreground tabular-nums">{formatBytes(file.size)}</span>
        </div>
        <div className="h-1.5 overflow-hidden rounded-full bg-muted">
          <div className="h-full bg-primary transition-[width]" style={{ width: `${Math.round(progress * 100)}%` }} />
        </div>
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <span role="status">
            {progress < 1
              ? `Sending to ${destination?.label ?? "the machine"}… ${Math.round(progress * 100)}%`
              : "Going live…"}
          </span>
          <button
            type="button"
            onClick={() => cancel.current?.()}
            className="underline-offset-4 hover:text-foreground hover:underline"
          >
            Cancel
          </button>
        </div>
      </div>
    )

  return (
    <>
      {/* The defaults, each changeable in place; dropping is the only required step. */}
      <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-sm text-muted-foreground">
        <span>Hosted on</span>
        <Choice
          label={destination?.label ?? (machines.isPending ? "…" : (source.machine?.alias ?? "no machine"))}
          value={destination ? destinationValue(destination) : ""}
          options={destinations.map((entry) => ({ value: destinationValue(entry), label: entry.label }))}
          onChange={setChosen}
        />
        <span aria-hidden>·</span>
        <SettingsChoices settings={settings} />
      </div>
      <button
        type="button"
        data-file-drop
        disabled={!destination}
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
          const dropped = event.dataTransfer.files[0]
          if (dropped) start(dropped)
        }}
        className={cn(
          "flex h-44 flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-input bg-surface text-sm transition-colors outline-none hover:border-ring/60 focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none disabled:opacity-50",
          over && "border-ring bg-ring/5",
        )}
      >
        <UploadSimpleIcon className="size-5 text-muted-foreground" />
        <span className="font-medium">{over ? "Drop to share" : "Drop a file here"}</span>
        <span className="text-xs text-muted-foreground">or click to choose</span>
      </button>
      {error ? (
        <p className="text-sm text-destructive">{error}</p>
      ) : (
        <p className="text-xs text-muted-foreground">
          {destination
            ? "Web pages, Markdown, PDFs and images open in the browser; other files download."
            : source.machine
              ? `${source.machine.alias} cannot host files right now. Bring it online to share.`
              : "No machine can host files right now. Bring a machine online to share."}
        </p>
      )}
      <input
        ref={inputRef}
        type="file"
        hidden
        onChange={(event) => {
          const chosenFile = event.currentTarget.files?.[0]
          event.currentTarget.value = ""
          if (chosenFile) start(chosenFile)
        }}
      />
    </>
  )
}
