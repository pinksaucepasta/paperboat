import "@wterm/dom/css"

import { WTerm } from "@wterm/dom"
import { GhosttyCore } from "@wterm/ghostty"
import type { TerminalConnectionController } from "@/lib/terminal/connection-controller"
import { classifyFilePaste, filePasteMessage } from "@/lib/terminal/file-paste"
import { paletteSequence } from "@/lib/terminal/themes"

/**
 * Ghostty's VT core and wterm's renderer in the caller's host element, wired to the controller:
 * input, size and title go to it; file paste and drop become uploads. Returns the cleanup.
 */
export function mountTerminalRenderer(host: HTMLDivElement, controller: TerminalConnectionController): () => void {
  let disposed = false
  let terminal: WTerm | null = null
  let core: GhosttyCore | null = null
  let observer: ResizeObserver | null = null
  let fitting = false
  let dragDepth = 0

  const fit = () => {
    if (!terminal || !host.isConnected) return
    const style = getComputedStyle(host)
    const cellWidth = Number.parseFloat(style.getPropertyValue("--term-cell-width"))
    const rowHeight = Number.parseFloat(style.getPropertyValue("--term-row-height"))
    if (!(cellWidth > 0) || !(rowHeight > 0)) return
    const columns = Math.min(500, Math.max(1, Math.floor(host.clientWidth / cellWidth)))
    const rows = Math.min(200, Math.max(1, Math.floor(host.clientHeight / rowHeight)))
    if (columns === terminal.cols && rows === terminal.rows) return
    fitting = true
    try {
      terminal.resize(columns, rows)
    } finally {
      fitting = false
    }
  }
  const takeFiles = (event: Event, selection: ReturnType<typeof classifyFilePaste>) => {
    if (selection.kind === "text") return
    event.preventDefault()
    event.stopImmediatePropagation()
    if (selection.kind === "files") void controller.uploadFiles(selection.files)
    else controller.showUploadNotice(filePasteMessage(selection.kind))
  }
  const onPasteCapture = (event: ClipboardEvent) => takeFiles(event, classifyFilePaste(event.clipboardData))
  const hasFiles = (transfer: DataTransfer | null) => !!transfer && Array.from(transfer.types ?? []).includes("Files")
  const onDragEnter = (event: DragEvent) => {
    if (!hasFiles(event.dataTransfer)) return
    event.preventDefault()
    dragDepth++
    controller.setDragOver(true)
  }
  const onDragOver = (event: DragEvent) => {
    if (!hasFiles(event.dataTransfer)) return
    event.preventDefault()
    if (event.dataTransfer) event.dataTransfer.dropEffect = "copy"
    controller.setDragOver(true)
  }
  const onDragLeave = (event: DragEvent) => {
    if (!hasFiles(event.dataTransfer)) return
    dragDepth = Math.max(0, dragDepth - 1)
    if (dragDepth === 0) controller.setDragOver(false)
  }
  const onDrop = (event: DragEvent) => {
    const selection = classifyFilePaste(event.dataTransfer)
    dragDepth = 0
    controller.setDragOver(false)
    takeFiles(event, selection)
  }
  const listen = () => {
    document.addEventListener("visibilitychange", controller.syncVisibility)
    window.addEventListener("pagehide", controller.pageHidden)
    window.addEventListener("pageshow", controller.syncVisibility)
    // A network change can kill the socket without a close; the same cursor probe finds out.
    window.addEventListener("online", controller.syncVisibility)
    host.addEventListener("paste", onPasteCapture, true)
    host.addEventListener("dragenter", onDragEnter, true)
    host.addEventListener("dragover", onDragOver, true)
    host.addEventListener("dragleave", onDragLeave, true)
    host.addEventListener("drop", onDrop, true)
  }
  const unlisten = () => {
    document.removeEventListener("visibilitychange", controller.syncVisibility)
    window.removeEventListener("pagehide", controller.pageHidden)
    window.removeEventListener("pageshow", controller.syncVisibility)
    window.removeEventListener("online", controller.syncVisibility)
    host.removeEventListener("paste", onPasteCapture, true)
    host.removeEventListener("dragenter", onDragEnter, true)
    host.removeEventListener("dragover", onDragOver, true)
    host.removeEventListener("dragleave", onDragLeave, true)
    host.removeEventListener("drop", onDrop, true)
  }

  void (async () => {
    try {
      const theme = controller.getTheme()
      core = await GhosttyCore.load({
        wasmPath: "/terminal/ghostty-vt-0.5.1.wasm",
        scrollbackLimit: 65536,
        foregroundColor: theme.foreground,
        backgroundColor: theme.background,
      })
      if (disposed) {
        core.dispose()
        return
      }
      terminal = new WTerm(host, {
        core,
        autoResize: false,
        cursorBlink: true,
        onData: controller.typed,
        onBinary: controller.sendInput,
        onTitle: controller.setTitle,
        onResize: (columns, rows) => {
          if (fitting) controller.screenResized(columns, rows)
        },
      })
      await terminal.init()
      terminal.write(paletteSequence(controller.getTheme()))
      if (disposed) {
        terminal.destroy()
        core.dispose()
        return
      }
      host.style.height = "100%"
      const input = host.querySelector("textarea")
      if (input) {
        input.setAttribute("autocapitalize", "none")
        input.setAttribute("enterkeyhint", "enter")
        if (/Android/i.test(navigator.userAgent)) input.setAttribute("inputmode", "url")
        if (matchMedia("(pointer: coarse)").matches) input.blur()
      }
      const live = terminal
      const liveCore = core
      observer = new ResizeObserver(() => {
        if (controller.canFit()) fit()
      })
      observer.observe(host)
      listen()
      controller.attachScreen({
        write: (data) => live.write(data),
        resize: (columns, rows) => live.resize(columns, rows),
        textarea: () => live.element.querySelector("textarea"),
        cursorKeysApp: () => liveCore.cursorKeysApp(),
        fit,
      })
    } catch (reason) {
      terminal?.destroy()
      core?.dispose()
      if (disposed) return
      controller.rendererFailed(reason)
    }
  })()

  return () => {
    disposed = true
    observer?.disconnect()
    unlisten()
    controller.detachScreen()
    terminal?.destroy()
    core?.dispose()
  }
}
