"use client"

import { UploadSimpleIcon } from "@phosphor-icons/react"
import { useEffect, useRef, useState } from "react"

/**
 * Files dropped anywhere on the page (Previews shares one, Files sends them to an Inbox). Returns
 * the dropped files (for the dialog) and whether files are being dragged over (for the overlay).
 * A dialog's own drop zone (`data-file-drop`) keeps its drops to itself.
 */
export function usePageFileDrop() {
  const [files, setFiles] = useState<File[]>([])
  const [over, setOver] = useState(false)
  const depth = useRef(0)
  useEffect(() => {
    const hasFiles = (event: DragEvent) => event.dataTransfer?.types.includes("Files") ?? false
    const enter = (event: DragEvent) => {
      if (!hasFiles(event)) return
      depth.current++
      setOver(true)
    }
    const leave = (event: DragEvent) => {
      if (!hasFiles(event)) return
      depth.current = Math.max(0, depth.current - 1)
      if (depth.current === 0) setOver(false)
    }
    const overPage = (event: DragEvent) => {
      if (!hasFiles(event)) return
      event.preventDefault()
      if (event.dataTransfer) event.dataTransfer.dropEffect = "copy"
    }
    const drop = (event: DragEvent) => {
      if (!hasFiles(event)) return
      event.preventDefault()
      depth.current = 0
      setOver(false)
      if ((event.target as Element | null)?.closest?.("[data-file-drop]")) return
      const dropped = Array.from(event.dataTransfer?.files ?? [])
      if (dropped.length) setFiles(dropped)
    }
    window.addEventListener("dragenter", enter)
    window.addEventListener("dragleave", leave)
    window.addEventListener("dragover", overPage)
    window.addEventListener("drop", drop)
    return () => {
      window.removeEventListener("dragenter", enter)
      window.removeEventListener("dragleave", leave)
      window.removeEventListener("dragover", overPage)
      window.removeEventListener("drop", drop)
    }
  }, [])
  return { files, over, clear: () => setFiles([]) }
}

/** The page-wide hint while files are dragged over the page. */
export function DropOverlay({ title, detail }: { title: string; detail: string }) {
  return (
    <div className="pointer-events-none fixed inset-0 z-40 flex items-center justify-center bg-background/70 backdrop-blur-sm">
      <div className="flex flex-col items-center gap-2 rounded-xl border border-dashed border-ring bg-surface px-10 py-8 text-sm">
        <UploadSimpleIcon className="size-6 text-muted-foreground" />
        <span className="font-medium">{title}</span>
        <span className="text-xs text-muted-foreground">{detail}</span>
      </div>
    </div>
  )
}
