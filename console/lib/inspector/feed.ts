"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { idempotencyKey } from "@/lib/api"
import { InspectorError, type useInspectorChannel } from "@/lib/inspector/channel"

/** One capture as the daemon's inspector API sends it (`inspectorapi.recordWire`). */
export type Capture = {
  id: string
  method: string
  url: string
  url_truncated: boolean
  request_headers: Record<string, string[]> | null
  response_headers: Record<string, string[]> | null
  headers_truncated: boolean
  request_body?: string
  request_body_state: string
  response_body?: string
  response_body_state: string
  response_status: number
  error_code: string
  state: string
  replay_ineligible?: string
  /** The capture this record replayed; set by the machine. */
  replay_of?: string
  started_at: string
  finished_at: string
}

type Page = { records: Capture[]; next_cursor: string }
type Replay = { replay_record_id?: string; response_status?: number; code?: string }

/** "on"/"off" come from the daemon; it has no read of the policy, so "unknown" until the first list. */
export type CaptureState = "unknown" | "on" | "off"

type Channel = ReturnType<typeof useInspectorChannel>

const MAX_KEPT = 500
const scopeOf = (previewId: string) => ({ kind: "preview", resource: previewId, route: previewId })

/**
 * The live request list. Polls the daemon every second while the tab is visible and not paused,
 * from the last record seen (the daemon lists oldest first and the cursor is a record id), and
 * keys records by id, so reconnects and overlapping polls never duplicate a row.
 */
export function useCaptureFeed(channel: Channel, previewId: string, paused: boolean) {
  const [records, setRecords] = useState<Map<string, Capture>>(() => new Map())
  const [capture, setCapture] = useState<CaptureState>("unknown")
  const [bodies, setBodies] = useState(false)
  const cursor = useRef("")
  const busy = useRef(false)
  // Read after each await: a page that lands after Pause is dropped without moving the cursor,
  // so the list holds still and Resume fetches it again.
  const held = useRef(paused)
  useEffect(() => {
    held.current = paused
  }, [paused])
  const { request, phase } = channel

  const merge = useCallback((page: Capture[], reset: boolean) => {
    setRecords((current) => {
      const next = new Map(reset ? [] : current)
      for (const record of page) next.set(record.id, record)
      // Bounded: the daemon keeps 200 per preview; drop the oldest beyond that here too.
      while (next.size > MAX_KEPT) next.delete(next.keys().next().value!)
      return next
    })
    if (page.some((record) => ["complete", "truncated"].includes(record.request_body_state || record.response_body_state)))
      setBodies(true)
  }, [])

  const poll = useCallback(async () => {
    if (busy.current || held.current) return
    busy.current = true
    try {
      for (let pages = 0; pages < 10; pages++) {
        const reset = cursor.current === ""
        const page = (await request("inspect", "GET", "/v1/inspector/records", {
          ...scopeOf(previewId),
          cursor: cursor.current,
          limit: "100",
        })) as Page
        setCapture("on")
        if (held.current) break
        if (page.records.length) cursor.current = page.records[page.records.length - 1].id
        if (reset || page.records.length) merge(page.records, reset)
        if (!page.next_cursor) break
      }
    } catch (error) {
      // Channel failures show as the channel's phase; the next tick retries.
      if (!(error instanceof InspectorError)) return
      if (error.code === "inspector_not_enabled") setCapture("off")
      // The cursor record expired (15 min retention): start over; ids keep rows unique.
      if (error.code === "invalid_inspector_request") cursor.current = ""
    } finally {
      busy.current = false
    }
  }, [merge, previewId, request])

  useEffect(() => {
    if (phase !== "ready" || paused) return
    let timer = 0
    const tick = () => {
      if (document.visibilityState === "visible") void poll()
    }
    tick()
    timer = window.setInterval(tick, 1_000)
    document.addEventListener("visibilitychange", tick)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener("visibilitychange", tick)
    }
  }, [phase, paused, poll])

  /** The full record (bodies may have finished after the list saw it). */
  const load = useCallback(
    async (id: string) => {
      const record = (await request("inspect", "GET", `/v1/inspector/records/${encodeURIComponent(id)}`, scopeOf(previewId))) as Capture
      setRecords((current) => (current.has(id) ? new Map(current).set(id, record) : current))
      return record
    },
    [previewId, request],
  )

  const setPolicy = useCallback(
    async (enabled: boolean, withBodies: boolean) => {
      await request("replay", "PUT", "/v1/inspector/policy", undefined, {
        resource_kind: "preview",
        resource_id: previewId,
        route_id: previewId,
        enabled,
        capture_request_body: withBodies,
        capture_response_body: withBodies,
        // Replay needs the raw request kept on the machine, so it rides with bodies.
        capture_raw: withBodies,
      })
      setCapture(enabled ? "on" : "off")
      setBodies(withBodies)
      if (enabled) void poll()
    },
    [poll, previewId, request],
  )

  /** Purging drops the daemon's policy with the captures; capture that was on stays on. */
  const clear = useCallback(async () => {
    await request("replay", "DELETE", "/v1/inspector/records", scopeOf(previewId))
    cursor.current = ""
    setRecords(new Map())
    if (capture === "on") await setPolicy(true, bodies)
  }, [bodies, capture, previewId, request, setPolicy])

  const replay = useCallback(
    async (captureId: string) => {
      const result = (await request("replay", "POST", "/v1/inspector/replay", undefined, {
        resource_kind: "preview",
        resource_id: previewId,
        route_id: previewId,
        capture_id: captureId,
        idempotency_key: idempotencyKey(),
      })) as Replay
      void poll()
      return result
    },
    [poll, previewId, request],
  )

  const list = [...records.values()].reverse()
  return { records: list, capture, bodies, load, setPolicy, clear, replay }
}
