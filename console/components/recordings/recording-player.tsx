"use client"

import { PauseIcon, PlayIcon } from "@phosphor-icons/react"
import { WTerm } from "@wterm/dom"
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react"
import { ApiError } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { formatClock, recordingMessage, type Cast } from "@/lib/recordings"
import { useRecordingReplay } from "@/lib/recordings-replay"
import { loadGhosttyCores } from "@/lib/terminal/ghostty-core"
import { paletteSequence, terminalThemeStyle } from "@/lib/terminal/themes"
import { useTerminalTheme } from "@/lib/terminal/use-terminal-theme"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { Button } from "@/components/ui/button"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

const speeds = [1, 2, 4, 8] as const

/** /recordings/<id>: loads the recording, then plays it in the terminal renderer, read-only. */
export function RecordingPlayer({ id }: { id: string }) {
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const [attempt, setAttempt] = useState(0)
  const load = useRecordingReplay(id, attempt, unlocked)
  return (
    <>
      <PageHeader>
        <CrumbLink href="/recordings">Recordings</CrumbLink>
        <Crumb>Replay</Crumb>
      </PageHeader>
      <main
        className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-4 px-4 py-6 md:px-8 md:py-8"
        data-recording-phase={unlocked ? load.phase : "locked"}
      >
        {!unlocked ? (
          <EnvUnlock title="Recordings are encrypted. Unlock your vault to play this one." />
        ) : load.phase === "failed" ? (
          <ReplayError error={load.error} retry={() => setAttempt(attempt + 1)} />
        ) : load.phase === "loading" ? (
          <p className="py-12 text-center text-sm text-muted-foreground" data-recording-loading>
            {load.total ? `Loading the recording… ${formatBytes(load.received)} of ${formatBytes(load.total)}` : "Opening the recording…"}
          </p>
        ) : (
          <Player cast={load.cast} />
        )}
      </main>
    </>
  )
}

/** Each failure says what happened and offers the one step that can help. */
function ReplayError({ error, retry }: { error: unknown; retry: () => void }) {
  const code = error instanceof ApiError ? error.code : ""
  // Waiting fixes these: the machine comes back, a link is re-issued, the connection is retried.
  const retryable = ["machine_unavailable", "credential_expired", "connection_lost", "connection_failed", "recordings_unavailable", "too_many_pending", "network", "timeout"].includes(code) || !code
  return (
    <div className="flex flex-col items-center gap-3 py-12 text-center text-sm text-muted-foreground" data-recording-error={code || "failed"}>
      <p>{recordingMessage(error)}</p>
      {retryable && (
        <Button variant="outline" size="sm" onClick={retry}>
          Retry
        </Button>
      )}
    </div>
  )
}

/** Output, and the size it was shown at, from the start of the recording up to `time`. */
function screenAt(cast: Cast, time: number) {
  let size = { columns: cast.width, rows: cast.height }
  let output = ""
  let next = 0
  for (; next < cast.events.length && cast.events[next].time <= time; next++) {
    const event = cast.events[next]
    if (event.code === "o") output += event.data
    else if (event.code === "r") size = parseSize(event.data) ?? size
  }
  return { size, output, next }
}

const parseSize = (data: string) => {
  const match = /^(\d{1,3})x(\d{1,3})$/.exec(data)
  return match ? { columns: Number(match[1]), rows: Number(match[2]) } : undefined
}

function Player({ cast }: { cast: Cast }) {
  const { theme } = useTerminalTheme()
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<WTerm | null>(null)
  const coresRef = useRef<Awaited<ReturnType<typeof loadGhosttyCores>> | null>(null)
  const nextRef = useRef(0)
  const clockRef = useRef(0)
  const buildRef = useRef(0)
  const [time, setTime] = useState(0)
  const [playing, setPlaying] = useState(true)
  const [speed, setSpeed] = useState<(typeof speeds)[number]>(1)
  const [ready, setReady] = useState(false)

  // A seek builds a fresh terminal at the target: Ghostty never forgets cells inside one instance (ghostty-core.ts).
  const seek = useCallback(
    async (target: number) => {
      const host = hostRef.current
      if (!host) return
      const build = ++buildRef.current
      coresRef.current ??= await loadGhosttyCores("/terminal/ghostty-vt-0.5.1.wasm")
      if (build !== buildRef.current) return
      const { size, output, next } = screenAt(cast, target)
      termRef.current?.destroy()
      host.replaceChildren()
      const core = coresRef.current({ scrollbackLimit: 10000, foregroundColor: theme.foreground, backgroundColor: theme.background })
      const term = new WTerm(host, { core, autoResize: false, cols: size.columns, rows: size.rows, cursorBlink: false })
      await term.init()
      if (build !== buildRef.current) return term.destroy()
      term.write(paletteSequence(theme))
      term.write(output)
      termRef.current = term
      nextRef.current = next
      clockRef.current = target
      setTime(target)
      setReady(true)
    },
    [cast, theme],
  )

  useEffect(() => {
    void seek(clockRef.current)
    const builds = buildRef
    const term = termRef
    return () => {
      builds.current++ // a build still loading drops itself
      term.current?.destroy()
      term.current = null
    }
  }, [seek])

  useEffect(() => {
    if (!playing || !ready) return
    let last = performance.now()
    let frame = requestAnimationFrame(function tick(now) {
      const term = termRef.current
      clockRef.current = Math.min(cast.duration, clockRef.current + ((now - last) / 1000) * speed)
      last = now
      let output = ""
      for (; nextRef.current < cast.events.length && cast.events[nextRef.current].time <= clockRef.current; nextRef.current++) {
        const event = cast.events[nextRef.current]
        if (event.code === "o") output += event.data
        else if (event.code === "r") {
          const size = parseSize(event.data)
          if (size) term?.resize(size.columns, size.rows)
        }
      }
      if (output) term?.write(output)
      setTime(clockRef.current)
      if (clockRef.current >= cast.duration) return setPlaying(false)
      frame = requestAnimationFrame(tick)
    })
    return () => cancelAnimationFrame(frame)
  }, [playing, ready, speed, cast])

  const toggle = () => {
    if (!playing && clockRef.current >= cast.duration) void seek(0)
    setPlaying(!playing)
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3" data-recording-player>
      <div className="min-h-[24rem] flex-1 overflow-auto rounded-lg px-3 pt-2 pb-1" style={{ background: theme.background }}>
        <div ref={hostRef} className="terminal-host" style={terminalThemeStyle(theme) as React.CSSProperties} />
      </div>
      <div className="flex flex-wrap items-center gap-3 text-sm">
        <Button
          variant="outline"
          size="icon"
          aria-label={playing ? "Pause" : "Play"}
          data-recording-toggle={playing ? "pause" : "play"}
          disabled={!ready}
          onClick={toggle}
        >
          {playing ? <PauseIcon /> : <PlayIcon />}
        </Button>
        <input
          type="range"
          aria-label="Position"
          className="min-w-40 flex-1 accent-(--brand)"
          min={0}
          max={cast.duration || 0}
          step={0.1}
          value={time}
          disabled={!ready}
          onChange={(event) => void seek(Number(event.target.value))}
        />
        <span className="text-muted-foreground tabular-nums" data-recording-time>
          {formatClock(time)} / {formatClock(cast.duration)}
        </span>
        <ToggleGroup
          value={[String(speed)]}
          onValueChange={(next) => next[0] && setSpeed(Number(next[0]) as (typeof speeds)[number])}
          variant="outline"
          spacing={0}
          aria-label="Speed"
        >
          {speeds.map((value) => (
            <ToggleGroupItem key={value} value={String(value)} className="tabular-nums">
              {value}×
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      </div>
    </div>
  )
}
