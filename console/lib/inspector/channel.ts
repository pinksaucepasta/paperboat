"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { api, ApiError } from "@/lib/api"
import { verifyTicketKey } from "@/lib/terminal/browser-root-key"
import { loadBrowserTerminal, type BrowserTerminalAPI, type BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"

/**
 * The console side of a preview's inspector (PLAN F17). Captures stay on the machine: the
 * server only issues per-action grants and a single-use ticket for a machine-scoped E2E channel;
 * every capture read and write travels inside that channel to the daemon's inspector API.
 */

export type InspectorAction = "inspect" | "replay"

/** Where the channel is. Everything other than `ready` is shown as a full-page state. */
export type ChannelPhase =
  | "connecting"
  | "ready"
  | "reconnecting"
  | "offline" // 409 machine_unavailable: retried with backoff
  | "denied" // no inspect right, or access ended mid-view: final
  | "update" // the machine's daemon lacks the inspector capability: final
  | "gone" // the preview no longer exists: final

export type DaemonReply = { status: number; body: unknown }

/** A daemon error (`{code}` envelope) or a channel failure, with a code callers can branch on. */
export class InspectorError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
  }
}

type Grant = { credential_id: string; token: string; expires_at: string }
type Grants = { inspect: Grant; replay?: Grant }

type Ticket = {
  stream_url: string
  subprotocols: string[]
  expires_at: string
  role: string
  machine_id: string
  owner_account_id: string
  root_key_id: string
  root_public_key: string
  root_fingerprint: string
}

const GRANT_TTL = 300
const json = (body: unknown): RequestInit => ({
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify(body),
  cache: "no-store",
})
const BACKOFF = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000]

async function issue(previewId: string, action: InspectorAction): Promise<Grant> {
  return api<Grant>(
    "inspector/credentials",
    json({ resource_kind: "preview", resource_id: previewId, route_id: previewId, action, ttl_seconds: GRANT_TTL }),
  )
}

function revoke(grant: Grant | undefined) {
  if (!grant) return
  void api(`inspector/credentials/${encodeURIComponent(grant.credential_id)}`, {
    method: "DELETE",
    keepalive: true,
  }).catch(() => undefined)
}

/** Inspect is required; replay is optional (403 means this person may look but not replay). */
async function issueGrants(previewId: string): Promise<Grants> {
  const inspect = await issue(previewId, "inspect")
  try {
    return { inspect, replay: await issue(previewId, "replay") }
  } catch (error) {
    if (error instanceof ApiError && error.status === 403) return { inspect }
    revoke(inspect)
    throw error
  }
}

function checkTicket(ticket: Ticket, machineId: string) {
  if (ticket.role !== "inspector") throw new Error("The inspector ticket has an invalid role.")
  if (ticket.machine_id !== machineId) throw new Error("The inspector ticket is for a different machine.")
  if (
    !Array.isArray(ticket.subprotocols) ||
    ticket.subprotocols.length !== 2 ||
    !ticket.subprotocols[1].startsWith("pb-ticket.")
  )
    throw new Error("The encrypted inspector ticket is invalid.")
  const expiresAt = Date.parse(ticket.expires_at)
  if (!Number.isFinite(expiresAt) || expiresAt <= Date.now())
    throw new Error("The ticket expired before connecting. Retrying.")
  const url = new URL(ticket.stream_url)
  if (url.protocol !== "wss:" || url.username || url.password || url.hash)
    throw new Error("The inspector service did not provide a secure address.")
  return url
}

/** Final phases from a server or channel error code; undefined means retry. */
function finalPhase(error: unknown): ChannelPhase | undefined {
  const code = error instanceof ApiError || error instanceof InspectorError ? error.code : ""
  const status = error instanceof ApiError ? error.status : 0
  if (code === "capability_required") return "update"
  if (code === "machine_unavailable") return "offline"
  if (status === 404) return "gone"
  if (code === "inspector_not_authorized" || status === 401 || status === 403) return "denied"
  return undefined
}

export function useInspectorChannel(previewId: string, machineId: string, enabled: boolean) {
  const [phase, setPhase] = useState<ChannelPhase>("connecting")
  const [message, setMessage] = useState("")
  const [canReplay, setCanReplay] = useState(false)
  const grants = useRef<Grants | null>(null)
  const browser = useRef<BrowserTerminalAPI | null>(null)
  const connection = useRef("")
  const attempt = useRef(0)
  const timer = useRef<number | undefined>(undefined)
  const disposed = useRef(false)
  const startRef = useRef<() => void>(() => undefined)

  const retry = useCallback((reason: string, next: ChannelPhase = "reconnecting") => {
    if (disposed.current) return
    setPhase(next)
    setMessage(reason)
    window.clearTimeout(timer.current)
    const delay = BACKOFF[Math.min(attempt.current, BACKOFF.length - 1)]
    attempt.current++
    timer.current = window.setTimeout(() => startRef.current(), delay)
  }, [])

  const onEvent = useCallback(
    (id: string, event: BrowserTerminalEvent) => {
      if (disposed.current || event.type !== "state" || (connection.current && id && connection.current !== id)) return
      if (event.state === "connected") {
        attempt.current = 0
        setPhase("ready")
        setMessage("")
        return
      }
      if (event.state === "denied") {
        connection.current = ""
        setPhase(event.code === "capability_required" ? "update" : "denied")
        setMessage(event.message ?? "")
        return
      }
      if (["reconnecting", "closed", "transport-error"].includes(event.state ?? "")) {
        connection.current = ""
        // A machine running an older Paperboat refuses the inspector channel
        // before it opens, which looks like a transport error every time.
        retry(
          attempt.current >= 2
            ? "Still cannot open the encrypted channel. If this continues, update Paperboat on the machine (pb update)."
            : "The connection to the machine was lost. Reconnecting…",
        )
      }
    },
    [retry],
  )

  const start = useCallback(async () => {
    if (disposed.current) return
    let identityID = ""
    // Grants issued before a policy save are fenced by it; only a refusal of
    // freshly issued grants means access really ended.
    const fresh = grants.current === null
    try {
      const runtime = (browser.current ??= await loadBrowserTerminal())
      grants.current ??= await issueGrants(previewId)
      setCanReplay(Boolean(grants.current.replay))
      const identity = await runtime.createIdentity()
      if ("error" in identity) throw new Error(identity.message)
      identityID = identity.id
      const ticket = await api<Ticket>(
        "inspector/browser-ticket",
        json({ credential_id: grants.current.inspect.credential_id, browser_public_key_sha256: identity.public_key_sha256 }),
      )
      const url = checkTicket(ticket, machineId)
      const pin = await verifyTicketKey(
        runtime,
        ticket.owner_account_id,
        ticket.root_public_key,
        ticket.root_fingerprint,
        ticket.root_key_id,
      )
      if (disposed.current) return
      let started = ""
      const result = await runtime.connect(
        identity.id,
        {
          url: url.href,
          subprotocols: ticket.subprotocols,
          owner_account_id: ticket.owner_account_id,
          machine_id: machineId,
          root_key_id: pin.rootKeyID,
          root_public_key: pin.publicKey,
          root_fingerprint: pin.fingerprint,
          role: "inspector",
          mode: "inspector",
        },
        (event) => onEvent(started, event),
      )
      identityID = ""
      if (!result.id) throw new InspectorError(0, result.error ?? "connection_failed", result.message ?? "")
      started = result.id
      if (disposed.current) return runtime.leave(started)
      connection.current = started
    } catch (error) {
      if (identityID) browser.current?.discardIdentity(identityID)
      if (disposed.current) return
      const final = finalPhase(error)
      const text = error instanceof Error ? error.message : "The inspector could not connect."
      if (final === "denied" && !fresh && grants.current) {
        revoke(grants.current.inspect)
        revoke(grants.current.replay)
        grants.current = null
        retry("Access changed. Reconnecting with fresh permissions…")
      } else if (final === "offline") retry(text, "offline")
      else if (final) {
        setPhase(final)
        setMessage(text)
      } else retry(text)
    }
  }, [machineId, onEvent, previewId, retry])

  useEffect(() => {
    startRef.current = () => void start()
  }, [start])

  useEffect(() => {
    if (!enabled) return
    disposed.current = false
    // Out of the render pass: the first connect sets state.
    timer.current = window.setTimeout(() => startRef.current(), 0)
    return () => {
      disposed.current = true
      window.clearTimeout(timer.current)
      if (connection.current) browser.current?.leave(connection.current)
      connection.current = ""
      revoke(grants.current?.inspect)
      revoke(grants.current?.replay)
      grants.current = null
      // A paused channel is not ready; it reconnects when enabled again.
      setPhase("connecting")
    }
  }, [enabled])

  // Grants last five minutes; swap in fresh ones a minute early and revoke the old ones.
  useEffect(() => {
    if (!enabled) return
    const refresh = window.setInterval(async () => {
      const current = grants.current
      if (!current || Date.parse(current.inspect.expires_at) - Date.now() > 60_000) return
      try {
        const next = await issueGrants(previewId)
        if (disposed.current) return [next.inspect, next.replay].forEach(revoke)
        grants.current = next
        setCanReplay(Boolean(next.replay))
        revoke(current.inspect)
        revoke(current.replay)
      } catch (error) {
        const final = finalPhase(error)
        if (final && final !== "offline") {
          setPhase(final)
          setMessage(error instanceof Error ? error.message : "")
        }
      }
    }, 15_000)
    return () => window.clearInterval(refresh)
  }, [enabled, previewId])

  /** One daemon inspector API call over the E2E channel, with the grant for `action`. */
  /**
   * The machine refused a grant: revoked, or fenced by a policy save.
   * Reconnect with fresh grants; if access really ended, the server refuses
   * them and the page says so.
   */
  const refreshAccess = useCallback(() => {
    if (connection.current) browser.current?.leave(connection.current)
    connection.current = ""
    revoke(grants.current?.inspect)
    revoke(grants.current?.replay)
    grants.current = null
    retry("Access changed. Reconnecting with fresh permissions…")
  }, [retry])

  const request = useCallback(
    async (action: InspectorAction, method: string, path: string, query?: Record<string, string>, body?: unknown) => {
      const grant = grants.current?.[action]
      const id = connection.current
      if (!grant || !id || !browser.current) throw new InspectorError(0, "not_connected", "Not connected to the machine.")
      const payload: Record<string, unknown> = { grant: grant.token, method, path }
      if (query) payload.query = query
      if (body !== undefined) payload.body = body
      const reply = await browser.current.inspectorRequest(id, payload)
      if (reply.error) {
        // The machine re-checks every grant live; a refusal means access ended.
        if (reply.error === "not_found_or_forbidden") refreshAccess()
        throw new InspectorError(0, reply.error, reply.message ?? "The machine did not answer.")
      }
      const result = reply.result as DaemonReply
      if (result.status >= 400) {
        const code = (result.body as { code?: string } | null)?.code ?? "inspector_unavailable"
        // The daemon re-checks the grant live: losing the right ends the view.
        if (code === "inspector_forbidden" || code === "inspector_unauthorized") refreshAccess()
        throw new InspectorError(result.status, code, code.replaceAll("_", " "))
      }
      return result.body
    },
    [refreshAccess],
  )

  return { phase, message, canReplay, request }
}
