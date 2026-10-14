"use client"

import { UploadSimpleIcon } from "@phosphor-icons/react"
import { useRef, useState } from "react"
import { cn } from "@/lib/utils"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"

/**
 * Upload files to the terminal's machine: drop them or choose them. They land in the machine's
 * Paperboat Inbox over the terminal's encrypted connection, and their paths are typed into the
 * terminal, where the upload's progress shows.
 */
export function UploadDialog({
  machineName,
  open,
  onOpenChange,
  onFiles,
}: {
  machineName: string
  open: boolean
  onOpenChange: (open: boolean) => void
  onFiles: (files: File[]) => void
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)
  const take = (files: File[]) => {
    if (!files.length) return
    onOpenChange(false)
    onFiles(files)
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Upload to {machineName}</DialogTitle>
          <DialogDescription>
            Files go to the Paperboat Inbox on {machineName}, and their paths are typed into the terminal.
          </DialogDescription>
        </DialogHeader>
        <button
          type="button"
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
            take(Array.from(event.dataTransfer.files))
          }}
          className={cn(
            "flex h-44 flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-input bg-surface text-sm transition-colors outline-none hover:border-ring/60 focus-visible:ring-2 focus-visible:ring-ring/30",
            over && "border-ring bg-ring/5",
          )}
        >
          <UploadSimpleIcon className="size-5 text-muted-foreground" />
          <span className="font-medium">{over ? "Drop to upload" : "Drop files here"}</span>
          <span className="text-xs text-muted-foreground">or click to choose</span>
        </button>
        <p className="text-xs text-muted-foreground">You can also drop or paste files straight into the terminal.</p>
        <input
          ref={inputRef}
          type="file"
          multiple
          hidden
          onChange={(event) => {
            const files = Array.from(event.currentTarget.files ?? [])
            event.currentTarget.value = ""
            take(files)
          }}
        />
      </DialogContent>
    </Dialog>
  )
}
