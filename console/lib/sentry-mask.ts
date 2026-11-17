import type { ErrorEvent, EventHint } from "@sentry/core"
import { errorReference } from "@/lib/error-reference"

/**
 * What leaves the console in an error event, as an allowlist: the error's type, a scrubbed and bounded message, the
 * stack (file paths without origin, query or directory) and the ids support quotes. The message is kept only for the
 * console's own errors (ApiError and its kin, whose sentences it or the control plane wrote); any other error's text can
 * embed data (a JSON parse error quotes its input), so only its type is sent, as the CLI does. Everything else the SDK collects
 * (user, request and its URL and headers, breadcrumbs, extra data, component names, source lines, local variables) is
 * dropped, because it can hold terminal contents, ENV values, file names, join codes, emails or tokens
 * (mirrors cli/internal/diagnostics, which never sends an error's text at all).
 */
const keptContexts = ["browser", "os", "runtime"] as const
const keptTags = ["support_reference", "request_id", "digest", "runtime"] as const
const maxMessage = 200

const idPrefix = /^(support|client|request)_/
const patterns: [RegExp, string][] = [
  [/https?:\/\/[^\s"'<>)]+/gi, "[url]"],
  [/[\w.+-]+@[\w-]+(?:\.[\w-]+)+/g, "[email]"],
  [/(?:[A-Za-z]:\\|\/)(?:[^\s"'<>/\\]+[/\\])+[^\s"'<>/\\]*/g, "[path]"],
  [/\b[\w-]+\.(?:[A-Za-z]{1,6})\b/g, "[file]"],
  [/\b(?:Bearer|Basic)\s+\S+/gi, "[token]"],
]

/** A message with URLs, emails, paths, file names and token- or code-like words replaced; ids support quotes stay. */
export function scrubText(text: string): string {
  let out = text
  for (const [pattern, replacement] of patterns)
    out = out.replace(pattern, (match) => (idPrefix.test(match) ? match : replacement))
  out = out.replace(/[A-Za-z0-9_+/=-]{6,}/g, (word) => {
    if (idPrefix.test(word)) return word
    // Long runs are tokens or keys; shorter ones with a digit are join codes or ids.
    return word.length >= 20 || (/\d/.test(word) && /[A-Za-z]/.test(word)) || /^\d{6,}$/.test(word) ? "[id]" : word
  })
  return out.length > maxMessage ? `${out.slice(0, maxMessage)}…` : out
}

/** A stack frame's file as the source map needs it: its path under the app, never the origin, query or host directory. */
export function scrubFile(file: string | undefined): string | undefined {
  if (!file) return undefined
  const bare = file.replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]*/i, "").replace(/[?#].*$/, "")
  const next = bare.search(/(?:^|\/)(?:_next|\.next[\w-]*)\//)
  const tail = next >= 0 ? bare.slice(next).replace(/^\//, "") : bare.split(/[/\\]/).pop()
  return tail || undefined
}

type Frame = NonNullable<NonNullable<NonNullable<ErrorEvent["exception"]>["values"]>[number]["stacktrace"]>["frames"]

const frames = (list: Frame): Frame =>
  list?.map((frame) => ({
    filename: scrubFile(frame.filename),
    function: frame.function,
    lineno: frame.lineno,
    colno: frame.colno,
    in_app: frame.in_app,
    debug_id: frame.debug_id,
  }))

/** The console's own error sentences (ApiError, DeletionError) carry a string `code`; no engine error does. */
const authored = (error: unknown) => typeof (error as { code?: unknown } | undefined)?.code === "string"

/** The SDK's beforeSend: returns a fresh event holding only what is listed above, tagged with the error's reference. */
export function maskEvent(event: ErrorEvent, hint?: EventHint): ErrorEvent {
  const cause = hint?.originalException
  const tags: Record<string, string> = {}
  for (const key of keptTags) if (typeof event.tags?.[key] === "string") tags[key] = event.tags[key] as string
  tags.support_reference ??= errorReference(cause)
  const requestId = (cause as { requestId?: unknown } | undefined)?.requestId
  if (typeof requestId === "string" && /^[A-Za-z0-9_.:-]{1,128}$/.test(requestId)) tags.request_id ??= requestId
  const contexts: NonNullable<ErrorEvent["contexts"]> = {}
  for (const key of keptContexts) {
    const value = event.contexts?.[key]
    if (value) contexts[key] = value
  }
  return {
    event_id: event.event_id,
    timestamp: event.timestamp,
    platform: event.platform,
    level: event.level,
    release: event.release,
    environment: event.environment,
    sdk: event.sdk,
    debug_meta: event.debug_meta,
    type: event.type,
    exception: event.exception && {
      values: event.exception.values?.map((value) => ({
        type: value.type,
        value: value.value !== undefined && authored(cause) ? scrubText(value.value) : undefined,
        mechanism: value.mechanism && { type: value.mechanism.type, handled: value.mechanism.handled },
        stacktrace: value.stacktrace && { frames: frames(value.stacktrace.frames) },
      })),
    },
    tags,
    contexts,
  }
}
