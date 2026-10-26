"use client"

import { useCallback, useEffect, useState, useSyncExternalStore } from "react"
import { api } from "@/lib/api"
import { guestApi } from "@/lib/terminal/queries"
import { verifyTicketKey } from "@/lib/terminal/browser-root-key"
import { loadBrowserTerminal } from "@/lib/terminal/browser-terminal-wasm"
import { TerminalConnectionController } from "@/lib/terminal/connection-controller"
import {
  terminalCanAttach,
  type ControllerDeps,
  type TerminalSession,
  type TerminalTicket,
} from "@/lib/terminal/connection-types"
import { mountTerminalRenderer } from "@/lib/terminal/terminal-renderer"
import type { TerminalTheme } from "@/lib/terminal/themes"

export {
  terminalCanAttach,
  type HostParticipant,
  type TerminalPhase,
  type TerminalRole,
  type TerminalSession,
} from "@/lib/terminal/connection-types"

/**
 * The browser terminal for React: the connection controller's state plus its actions. The UI is
 * left to the caller, so the same terminal can fill a page or sit in a quick panel.
 *
 * The renderer depends on the session's id only, so a caller can keep polling the session
 * (participants, title) without tearing down a live terminal.
 */

const browserDeps: ControllerDeps = {
  loadBrowserTerminal,
  requestTicket: (accessMethod, sessionID, browserKeySHA256, signal) => {
    const path = `terminal-sessions/${encodeURIComponent(sessionID)}/${accessMethod === "code" ? "guest-browser-ticket" : "browser-ticket"}`
    return (accessMethod === "code" ? guestApi<TerminalTicket> : api<TerminalTicket>)(path, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ browser_public_key_sha256: browserKeySHA256 }),
      cache: "no-store",
      signal,
    })
  },
  verifyTicketKey,
  setTimeout: (callback, ms) => window.setTimeout(callback, ms),
  clearTimeout: (id) => window.clearTimeout(id),
  requestAnimationFrame: (callback) => void requestAnimationFrame(callback),
  hidden: () => document.hidden,
  now: () => Date.now(),
}

export function useTerminalConnection({
  session,
  theme,
  hostRef,
  accessMethod = "account",
}: {
  session: TerminalSession
  theme: TerminalTheme
  /** "code" for guests who joined with a join code: they get a guest ticket and have no account. */
  accessMethod?: "account" | "code"
  /** The element wterm renders into. The caller owns it, so the returned state holds no refs. */
  hostRef: React.RefObject<HTMLDivElement | null>
}) {
  const sessionID = session.id
  const [controller] = useState(
    () => new TerminalConnectionController({ session, theme, accessMethod, deps: browserDeps }),
  )
  const state = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot)

  useEffect(() => {
    controller.setSession(session)
  }, [controller, session])
  useEffect(() => {
    controller.setTheme(theme)
  }, [controller, theme])
  useEffect(() => {
    controller.setAccessMethod(accessMethod)
  }, [controller, accessMethod])

  // Renderer: one Ghostty core and wterm per session.
  useEffect(() => {
    const host = hostRef.current
    if (!host || !terminalCanAttach(session)) return
    return mountTerminalRenderer(host, controller)
    // The renderer follows the session's id only; see above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [controller, sessionID, hostRef])

  useEffect(() => {
    controller.mount()
    return () => controller.dispose()
  }, [controller])

  const reconnect = useCallback(() => {
    if (controller.hasScreen) void controller.connect()
    else window.location.reload()
  }, [controller])

  return {
    phase: state.phase,
    role: state.role,
    catchingUp: state.catchingUp,
    error: state.error,
    notice: state.notice,
    connectedEdge: state.connectedEdge,
    title: state.title,
    limit: state.limit,
    participants: state.participants,
    visibility: state.visibility,
    resizeControl: state.resizeControl,
    reconnects: state.reconnects,
    reconnect,
    fitToWindow: controller.fitToWindow,
    focus: controller.focus,
    upload: {
      progress: state.uploadProgress,
      notice: state.uploadNotice,
      dragOver: state.dragOver,
      busy: state.uploadBusy,
      start: controller.uploadFiles,
      cancel: controller.cancelUpload,
      dismiss: controller.dismissUploadNotice,
    },
    mobile: {
      modifiers: state.mobileModifiers,
      toggle: controller.toggleMobileModifier,
      send: controller.sendMobileKey,
    },
  }
}

export type TerminalConnection = ReturnType<typeof useTerminalConnection>
