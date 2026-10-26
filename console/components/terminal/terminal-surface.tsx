"use client"

import { CircleNotchIcon, UploadSimpleIcon, XIcon } from "@phosphor-icons/react"
import { cn } from "@/lib/utils"
import type { TerminalConnection } from "@/lib/terminal/use-terminal-connection"
import type { MobileKey } from "@/lib/terminal/mobile-keys"
import { terminalThemeStyle, type TerminalTheme } from "@/lib/terminal/themes"
import { LimitReached } from "@/components/limit-reached"
import { QuotaMessage } from "@/components/quota-banner"
import { Button } from "@/components/ui/button"

const mobileKeys: { label: string; key: MobileKey | "CTRL" | "ALT"; description: string }[] = [
  { label: "esc", key: "ESC", description: "Escape" },
  { label: "tab", key: "TAB", description: "Tab" },
  { label: "ctrl", key: "CTRL", description: "Control for the next key" },
  { label: "alt", key: "ALT", description: "Alt for the next key" },
  { label: "←", key: "LEFT", description: "Left arrow" },
  { label: "↑", key: "UP", description: "Up arrow" },
  { label: "↓", key: "DOWN", description: "Down arrow" },
  { label: "→", key: "RIGHT", description: "Right arrow" },
  { label: "/", key: "/", description: "Slash" },
  { label: "-", key: "-", description: "Hyphen" },
  { label: "home", key: "HOME", description: "Home" },
  { label: "end", key: "END", description: "End" },
  { label: "pgup", key: "PGUP", description: "Page up" },
  { label: "pgdn", key: "PGDN", description: "Page down" },
]

/**
 * The terminal itself, in the chosen theme, with what can sit over it: progress while it
 * connects or catches up, why it stopped, file drops and uploads. Pages and the quick terminal
 * put their own controls around it.
 */
export function TerminalSurface({
  connection,
  hostRef,
  theme,
  machineName,
  onLeave,
  className,
}: {
  connection: TerminalConnection
  hostRef: React.RefObject<HTMLDivElement | null>
  theme: TerminalTheme
  machineName: string
  /** Where "stopped" states send people, such as back to the terminal list. */
  onLeave?: { label: string; action: () => void }
  className?: string
}) {
  const { phase, error, upload, role, mobile } = connection
  const busy = phase === "connecting" || phase === "reconnecting"
  const stopped = phase === "denied" || phase === "closed" || phase === "transport-error" || phase === "quota" || phase === "limit"
  // Overlays take the theme's colours, so they belong to the terminal rather than the page.
  const overlay = {
    background: `color-mix(in oklab, ${theme.background} 92%, transparent)`,
    color: theme.foreground,
    borderColor: `color-mix(in oklab, ${theme.foreground} 16%, transparent)`,
  }
  const muted = { color: `color-mix(in oklab, ${theme.foreground} 62%, transparent)` }

  return (
    <div
      className={cn("relative flex min-h-0 flex-col", className)}
      style={{ background: theme.background }}
      data-terminal-phase={phase}
      data-terminal-role={role}
      data-terminal-reconnects={connection.reconnects}
      data-terminal-replays={connection.replays}
      data-terminal-resyncs={connection.resyncs}
      data-terminal-close={connection.lastClose || undefined}
    >
      <div className="relative min-h-0 flex-1 px-3 pt-2 pb-1">
        <div
          ref={hostRef}
          className="terminal-host h-full"
          style={terminalThemeStyle(theme) as React.CSSProperties}
          onClick={connection.focus}
        />

        {busy && (
          <div
            className="pointer-events-none absolute inset-x-0 top-3 flex justify-center"
            role="status"
            aria-live="polite"
          >
            <div
              className="flex items-center gap-2 rounded-full border px-3 py-1.5 text-xs shadow-sm backdrop-blur"
              style={overlay}
            >
              <CircleNotchIcon className="size-3.5 animate-spin" />
              {phase === "reconnecting" ? (error ? `Reconnecting: ${error}` : "Reconnecting") : "Connecting"}
              {phase === "reconnecting" && (
                <Button size="xs" variant="outline" className="pointer-events-auto ml-1" onClick={connection.reconnect}>
                  Reconnect now
                </Button>
              )}
            </div>
          </div>
        )}

        {stopped && (
          <div
            className="absolute inset-0 flex items-center justify-center p-6"
            style={{ background: overlay.background }}
          >
            <div className="w-full max-w-sm rounded-lg border p-5 text-sm" style={overlay}>
              <p className="font-medium">
                {phase === "closed"
                  ? "Terminal closed"
                  : phase === "denied"
                    ? "No access"
                    : phase === "quota"
                      ? "Bandwidth used up"
                      : phase === "limit"
                        ? "Browser terminal limit reached"
                        : "Connection lost"}
              </p>
              {phase === "quota" ? (
                <div className="mt-1.5" style={muted}>
                  <QuotaMessage />
                </div>
              ) : phase === "limit" && connection.limit ? (
                <div className="mt-1.5">
                  <LimitReached error={connection.limit} />
                </div>
              ) : error && (
                <p className="mt-1.5" style={muted}>
                  {error}
                </p>
              )}
              <div className="mt-4 flex gap-2">
                {phase === "transport-error" && (
                  <Button size="sm" onClick={connection.reconnect}>
                    Reconnect
                  </Button>
                )}
                {onLeave && (
                  <Button
                    size="sm"
                    variant={phase === "transport-error" ? "ghost" : "secondary"}
                    onClick={onLeave.action}
                  >
                    {onLeave.label}
                  </Button>
                )}
              </div>
            </div>
          </div>
        )}

        {upload.dragOver && (
          <div
            className="pointer-events-none absolute inset-3 grid place-items-center rounded-lg border-2 border-dashed text-sm font-medium"
            style={overlay}
            aria-hidden
          >
            <span className="flex items-center gap-2">
              <UploadSimpleIcon className="size-4" />
              Drop to upload to {machineName}
            </span>
          </div>
        )}

        {upload.progress && (
          <div
            className="absolute top-3 right-3 left-3 ml-auto flex max-w-sm items-center gap-3 rounded-lg border p-3 text-xs shadow-sm"
            style={overlay}
            role="status"
            aria-live="polite"
          >
            <div className="min-w-0 flex-1">
              <p className="font-medium">{uploadLabel(upload.progress)}</p>
              {upload.progress.phase !== "canceling" && (
                <div className="mt-2 h-1 overflow-hidden rounded-full" style={{ background: overlay.borderColor }}>
                  <div
                    className="h-full rounded-full transition-[width]"
                    style={{ width: `${uploadPercent(upload.progress)}%`, background: theme.foreground }}
                  />
                </div>
              )}
              <p className="mt-1.5" style={muted}>
                Typing is paused until the upload finishes.
              </p>
            </div>
            <Button
              size="sm"
              variant="ghost"
              disabled={upload.progress.phase === "canceling"}
              onClick={() => void upload.cancel()}
            >
              Cancel
            </Button>
          </div>
        )}

        {upload.notice && (
          <div
            className="absolute right-3 bottom-3 left-3 ml-auto flex max-w-sm items-start gap-2 rounded-lg border p-3 text-xs shadow-sm"
            style={overlay}
            role="status"
            aria-live="polite"
          >
            <span className="flex-1">{upload.notice}</span>
            <button type="button" aria-label="Dismiss" onClick={upload.dismiss} style={muted}>
              <XIcon className="size-3.5" />
            </button>
          </div>
        )}
      </div>

      {/* Keys phones lack, shown only on touch screens and only to people who can type. */}
      {role !== "viewer" && (
        <div
          className="no-scrollbar hidden shrink-0 gap-1 overflow-x-auto border-t px-2 py-1.5 pointer-coarse:flex"
          style={{ borderColor: overlay.borderColor }}
          role="group"
          aria-label="Extra keys"
        >
          {mobileKeys.map(({ label, key, description }) => {
            const pressed = key === "CTRL" ? mobile.modifiers.ctrl : key === "ALT" ? mobile.modifiers.alt : undefined
            return (
              <button
                key={key}
                type="button"
                aria-label={description}
                aria-pressed={pressed}
                disabled={phase !== "attached" || upload.busy}
                onPointerDown={(event) => event.preventDefault()}
                onClick={() =>
                  key === "CTRL" || key === "ALT" ? mobile.toggle(key === "CTRL" ? "ctrl" : "alt") : mobile.send(key)
                }
                className="h-8 min-w-10 shrink-0 rounded-md border px-2 font-mono text-xs disabled:opacity-40"
                style={{
                  borderColor: overlay.borderColor,
                  color: theme.foreground,
                  background: pressed ? overlay.borderColor : "transparent",
                }}
              >
                {label}
              </button>
            )
          })}
        </div>
      )}
      {connection.notice && (
        <div className="sr-only" aria-live="polite">
          {connection.notice}
        </div>
      )}
    </div>
  )
}

function uploadLabel(progress: NonNullable<TerminalConnection["upload"]["progress"]>): string {
  if (progress.phase === "checking") return "Checking transfer limits"
  if (progress.phase === "hashing") return `Preparing file ${progress.fileNumber} of ${progress.fileCount}`
  if (progress.phase === "uploading") return `Uploading file ${progress.fileNumber} of ${progress.fileCount}`
  if (progress.phase === "finishing") return "Verifying on the machine"
  return "Canceling upload"
}

function uploadPercent(progress: NonNullable<TerminalConnection["upload"]["progress"]>): number {
  if (progress.totalBytes) return Math.min(100, Math.round((progress.transferredBytes / progress.totalBytes) * 100))
  return progress.phase === "finishing" ? 100 : 0
}
