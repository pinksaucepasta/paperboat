"use client"

import { CaretDownIcon, FileIcon, UploadSimpleIcon } from "@phosphor-icons/react"
import { useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import { useMachines, useWorkspace, type Machine } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { opensInBrowser, shareFile, type Preview } from "@/lib/tunnels"
import { cn } from "@/lib/utils"
import { canPreview } from "@/components/tunnels/machine-choice"
import { Starting } from "@/components/tunnels/new-preview"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

/*
 * Drop a file, get a link (like Vercel's drop.new). The file goes to one of your machines' Paperboat
 * Inbox and that machine serves it at a preview address; Paperboat's servers pass it through and
 * keep nothing. Where it's hosted, who can open it and for how long are one line of defaults above
 * the drop zone, so dropping is the only step.
 */

const machineKey = "paperboat:share-file-machine"
const lifetimes = [
  { hours: 1, label: "1 hour" },
  { hours: 8, label: "8 hours" },
  { hours: 24, label: "24 hours" },
]
const audiences = { public: "Anyone with the link", private: "Only you" } as const

function rememberedMachine(): string | null {
  try {
    return localStorage.getItem(machineKey)
  } catch {
    return null
  }
}

function rememberMachine(id: string) {
  try {
    localStorage.setItem(machineKey, id)
  } catch {
    // Blocked storage: the next share starts from the default machine.
  }
}

export function ShareFileButton({ machine }: { machine?: Machine }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button variant="secondary" size="lg" onClick={() => setOpen(true)}>
        <UploadSimpleIcon data-icon="inline-start" />
        Share a file
      </Button>
      {open && <ShareFileDialog machine={machine} onClose={() => setOpen(false)} />}
    </>
  )
}

export function ShareFileDialog({
  machine,
  file: initialFile,
  onClose,
}: {
  machine?: Machine
  file?: File
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const machines = useMachines()
  const usable = (machines.data ?? []).filter(canPreview)
  const [chosen, setChosen] = useState<string | null>(() => machine?.id ?? rememberedMachine())
  const host = usable.find((entry) => entry.id === chosen) ?? usable[0]
  const [access, setAccess] = useState<Preview["access_mode"]>("public")
  const [hours, setHours] = useState(24)
  const [file, setFile] = useState<File>()
  const [progress, setProgress] = useState(0)
  const [previewId, setPreviewId] = useState<string>()
  const [error, setError] = useState<string>()
  const cancel = useRef<() => void>(undefined)
  const inputRef = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)

  const start = (next: File) => {
    if (!host) return
    setFile(next)
    setError(undefined)
    setProgress(0)
    rememberMachine(host.id)
    const upload = shareFile(
      { file: next, machineId: host.id, workspace: workspace?.id ?? "personal", access, hours },
      setProgress,
    )
    cancel.current = upload.cancel
    upload.done.then(
      (id) => {
        setPreviewId(id)
        void queryClient.invalidateQueries({ queryKey: ["previews"] })
      },
      (failure: Error) => {
        setFile(undefined)
        setError(failure.message)
      },
    )
  }

  // A file dropped on the page starts as soon as there's a machine to host it.
  const started = useRef(false)
  useEffect(() => {
    if (!initialFile || started.current || !host) return
    started.current = true
    start(initialFile)
  })

  const uploading = Boolean(file) && !previewId
  const opens = file && opensInBrowser(file)

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (next) return
        if (uploading) cancel.current?.()
        onClose()
      }}
    >
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Share a file</DialogTitle>
        </DialogHeader>

        {previewId ? (
          <Starting
            id={previewId}
            machineName={host?.alias ?? "the machine"}
            note={opens ? "Opens in the browser." : "Downloads when opened."}
            label={file?.name}
            onDone={onClose}
          />
        ) : file ? (
          <div className="flex flex-col gap-3 py-2">
            <div className="flex min-w-0 items-center gap-3 rounded-lg bg-surface p-3 text-sm">
              <FileIcon className="size-5 shrink-0 text-muted-foreground" />
              <span className="min-w-0 flex-1 truncate font-medium">{file.name}</span>
              <span className="shrink-0 text-xs text-muted-foreground tabular-nums">{formatBytes(file.size)}</span>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-muted">
              <div
                className="h-full bg-primary transition-[width]"
                style={{ width: `${Math.round(progress * 100)}%` }}
              />
            </div>
            <div className="flex items-center justify-between text-xs text-muted-foreground">
              <span role="status">
                {progress < 1
                  ? `Sending to ${host?.alias ?? "the machine"}… ${Math.round(progress * 100)}%`
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
        ) : (
          <>
            {/* The defaults, each changeable in place; dropping is the only required step. */}
            <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-sm text-muted-foreground">
              <span>Hosted on</span>
              <Choice
                label={host?.alias ?? (machines.isPending ? "…" : "no machine")}
                value={host?.id ?? ""}
                options={usable.map((entry) => ({ value: entry.id, label: entry.alias }))}
                onChange={setChosen}
              />
              <span aria-hidden>·</span>
              <Choice
                label={audiences[access]}
                value={access}
                options={Object.entries(audiences).map(([value, label]) => ({ value, label }))}
                onChange={(value) => setAccess(value as Preview["access_mode"])}
              />
              <span aria-hidden>·</span>
              <Choice
                label={lifetimes.find((entry) => entry.hours === hours)!.label}
                value={String(hours)}
                options={lifetimes.map((entry) => ({ value: String(entry.hours), label: entry.label }))}
                onChange={(value) => setHours(Number(value))}
              />
            </div>
            <button
              type="button"
              data-file-drop
              disabled={!host}
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
                {host
                  ? "Web pages, Markdown, PDFs and images open in the browser; other files download."
                  : "None of your machines can host files right now. Bring one online to share."}
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
        )}
      </DialogContent>
    </Dialog>
  )
}

/** One editable default in the settings line: its value with a caret, a radio menu on click. */
export function Choice({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: string
  options: { value: string; label: string }[]
  onChange: (value: string) => void
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={options.length < 2}
        className="inline-flex items-center gap-0.5 rounded-sm font-medium text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none"
      >
        {label}
        {options.length > 1 && <CaretDownIcon className="size-3 text-muted-foreground" />}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-40">
        <DropdownMenuRadioGroup value={value} onValueChange={onChange}>
          {options.map((option) => (
            <DropdownMenuRadioItem key={option.value} value={option.value}>
              {option.label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
