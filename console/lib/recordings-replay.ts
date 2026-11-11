"use client"

import { useEffect, useState } from "react"
import { api, ApiError } from "@/lib/api"
import { vaultCrypto } from "@/lib/envvault/crypto"
import { parseCast, recordingVault, SegmentFramer, toBase64, type Cast, type Replay } from "@/lib/recordings"
import { verifyTicketKey } from "@/lib/terminal/browser-root-key"
import { loadBrowserTerminal, type BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"

/** The browser form of a replay answer carries the channel's connect fields. */
type BrowserReplay = Replay & Required<Pick<Replay, "stream_url" | "subprotocols" | "owner_account_id" | "root_key_id" | "root_public_key" | "root_fingerprint">>

export type ReplayLoad =
  | { phase: "loading"; received: number; total: number }
  | { phase: "ready"; cast: Cast }
  | { phase: "failed"; error: unknown }

/**
 * Loads one recording into memory: the server writes the replay audit event and answers with a single-use browser
 * ticket and the viewer's sealed key; the machine streams the ciphertext over the end-to-end channel; the vault
 * worker opens the key and every segment (checking their order and chain). Nothing is decrypted outside the worker.
 */
export function useRecordingReplay(id: string, attempt: number, enabled: boolean): ReplayLoad {
  const [load, setLoad] = useState<ReplayLoad>({ phase: "loading", received: 0, total: 0 })
  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let leave = () => {}
    let handle = ""
    const update = (next: ReplayLoad) => !stopped && setLoad(next)
    update({ phase: "loading", received: 0, total: 0 })
    void (async () => {
      const runtime = await loadBrowserTerminal()
      const identity = await runtime.createIdentity()
      if ("error" in identity) throw new Error(identity.message)
      const replay = await api<BrowserReplay>(`recordings/${encodeURIComponent(id)}/replay`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ browser_public_key_sha256: identity.public_key_sha256 }),
        cache: "no-store",
      })
      const opened = await recordingVault<{ handle: string }>({
        action: "recording-open",
        team_id: replay.team_id,
        recording_id: id,
        key_generation: replay.key_generation,
        viewer_key: replay.viewer_key,
        chain: replay.chain,
      })
      handle = opened.handle
      const pin = await verifyTicketKey(
        runtime,
        replay.owner_account_id,
        replay.root_public_key,
        replay.root_fingerprint,
        replay.root_key_id,
      )
      if (stopped) return
      const framer = new SegmentFramer()
      const texts: string[] = []
      let opening = Promise.resolve()
      let received = 0
      let total = 0
      let connection = ""
      const fail = (error: unknown) => {
        leave()
        update({ phase: "failed", error })
      }
      const onEvent = (event: BrowserTerminalEvent) => {
        if (stopped) return
        if (event.type === "state" && event.state === "connected")
          void runtime.recordingRead(connection, id).then((read) => {
            if (!read.result) return fail(new ApiError(0, read.error ?? "recording_unavailable", read.message ?? ""))
            total = read.result.bytes
            update({ phase: "loading", received, total })
          }, fail)
        else if (event.type === "output" && event.data && event.start_sequence) {
          try {
            const files = framer.push(Number(event.start_sequence), event.data)
            received += event.data.byteLength
            update({ phase: "loading", received, total })
            if (event.next_sequence) void runtime.ack(connection, event.next_sequence)
            // Segments open strictly in order: the worker's reader checks each against the one before.
            for (const file of files)
              opening = opening.then(async () => {
                const opened = await recordingVault<{ text: string }>({ action: "recording-segment", handle, file: toBase64(file) })
                texts.push(opened.text)
              })
          } catch (error) {
            fail(error)
          }
        } else if (event.type === "event" && (event.payload as { event?: string })?.event === "recording_stream_end")
          void opening.then(() => {
            if (framer.pending) throw new Error("The recording stream ended inside a segment.")
            leave()
            update({ phase: "ready", cast: parseCast(texts.join("")) })
          }).catch(fail)
        else if (event.type === "state" && ["denied", "closed", "transport-error", "reconnecting"].includes(event.state ?? ""))
          fail(new ApiError(0, event.code ?? "connection_lost", event.message ?? "The connection to the machine was lost."))
      }
      const result = await runtime.connect(
        identity.id,
        {
          url: replay.stream_url,
          subprotocols: replay.subprotocols,
          owner_account_id: replay.owner_account_id,
          machine_id: replay.machine_id,
          root_key_id: pin.rootKeyID,
          root_public_key: pin.publicKey,
          root_fingerprint: pin.fingerprint,
          role: "recording",
          mode: "recording",
        },
        onEvent,
      )
      if (!result.id) throw new ApiError(0, result.error ?? "connection_failed", result.message ?? "The machine couldn't be reached.")
      connection = result.id
      leave = () => runtime.leave(result.id!)
      if (stopped) leave()
    })().catch((error) => update({ phase: "failed", error }))
    return () => {
      stopped = true
      leave()
      if (handle) void vaultCrypto({ action: "recording-close", handle }).catch(() => undefined)
    }
  }, [id, attempt, enabled])
  return load
}
