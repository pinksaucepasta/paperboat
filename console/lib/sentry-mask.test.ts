import { afterAll, beforeAll, expect, test } from "bun:test"
import type { Envelope } from "@sentry/core"

/**
 * Mirrors cli/internal/diagnostics' redaction tests: every secret below is put where an SDK would collect it (the
 * error text, extra data, user, request, breadcrumbs, tags, contexts) and none may be in what is sent.
 */
const secrets = {
  terminal: "ls -la /home/user/private-notes; cat hunter2-password",
  envValue: "hunter2-ENVVALUE-4eC39HqLyjWDarjtT1zdp7dc",
  fileName: "quarterly-board-report.pdf",
  url: "https://console.example.test/terminals/term_abc123?code=JOINCODE9X&email=person@example.test",
  joinCode: "7KQ4M2XZ",
  email: "person@example.test",
  token: "pb_tok_0123456789abcdef0123456789abcdef",
}
const reference = "support_0b7c3a52-9d1e-4c8a-8f2b-3e5d7a9c1b64"
const sent: Envelope[] = []

process.env.NEXT_PUBLIC_SENTRY_DSN = "https://publickey@sentry.example.test/1"
// Bun resolves @sentry/nextjs to its Bun build (needs @sentry/bun); the browser SDK is the client path under test.
const Sentry = await import("@sentry/browser")
const { buildSentryOptions, sentryOptions } = await import("@/lib/sentry-options")
const { scrubFile, scrubText } = await import("@/lib/sentry-mask")
const { ApiError } = await import("@/lib/api")

beforeAll(() => {
  Sentry.init({
    ...sentryOptions,
    transport: () => ({
      send: async (envelope: Envelope) => {
        sent.push(envelope)
        return {}
      },
      flush: async () => true,
    }),
  })
})
afterAll(async () => {
  await Sentry.close()
})

const leaked = (blob: string) =>
  Object.entries(secrets)
    .filter(([, value]) => blob.includes(value))
    .map(([name]) => name)
const contaminate = (scope: import("@sentry/core").Scope) => {
  scope.setUser({ id: "u1", email: secrets.email, ip_address: "203.0.113.9" })
  scope.setExtra("pane", secrets.terminal)
  scope.setExtra("env", secrets.envValue)
  scope.setTag("file", secrets.fileName)
  scope.setTag("join", secrets.joinCode)
  scope.setContext("page", { url: secrets.url, token: secrets.token })
  scope.addBreadcrumb({ category: "console", message: secrets.terminal, data: { url: secrets.url } })
  scope.setSDKProcessingMetadata({
    normalizedRequest: { url: secrets.url, headers: { authorization: `Bearer ${secrets.token}` } },
  })
}
async function capture(error: unknown): Promise<string> {
  sent.length = 0
  Sentry.withScope((scope) => {
    contaminate(scope)
    Sentry.captureException(error)
  })
  await Sentry.flush(2000)
  return JSON.stringify(sent)
}

test("an error the console wrote leaks no secret from its text, scope, request or breadcrumbs, and keeps its reference", async () => {
  const error = new ApiError(
    503,
    "unavailable",
    `Couldn’t open ${secrets.fileName} at ${secrets.url} for ${secrets.email} with ${secrets.token} using code ${secrets.joinCode}`,
    undefined,
    undefined,
    { reference, requestId: "request_7" },
  )
  const blob = await capture(error)
  expect(blob).toContain("Couldn’t open [file] at [url] for [email]") // sent, scrubbed
  expect(leaked(blob)).toEqual([])
  expect(blob).toContain(reference)
  expect(blob).toContain("request_7")
  expect(blob).not.toContain("203.0.113.9")
})

test("another error sends its type and stack but not its text, which can quote its input", async () => {
  const blob = await capture(
    new SyntaxError(`Unexpected token in JSON: ${secrets.terminal} ${secrets.envValue} ${secrets.joinCode}`),
  )
  expect(blob).toContain("SyntaxError")
  expect(leaked(blob)).toEqual([])
  expect(blob).toMatch(/"support_reference":"client_[0-9a-f]{32}"/)
})

test("a message sent on its own and a string thrown are masked too", async () => {
  sent.length = 0
  Sentry.withScope((scope) => {
    contaminate(scope)
    Sentry.captureMessage(`${secrets.terminal} ${secrets.joinCode}`)
    Sentry.captureException(`${secrets.envValue} ${secrets.fileName}`)
  })
  await Sentry.flush(2000)
  expect(leaked(JSON.stringify(sent))).toEqual([])
})

test("stack frames keep the app path and drop the origin, query, host directory and source lines", () => {
  expect(scrubFile("https://console.example.test/_next/static/chunks/app-1a2b.js?v=secret#x")).toBe(
    "_next/static/chunks/app-1a2b.js",
  )
  expect(scrubFile("/home/user/paperboat/console/.next-a/server/chunks/ssr/x.js")).toBe(
    ".next-a/server/chunks/ssr/x.js",
  )
  expect(scrubFile("/home/user/private/dir/file.ts")).toBe("file.ts")
  expect(scrubText("see support_0b7c3a52-9d1e-4c8a-8f2b-3e5d7a9c1b64 and request_7")).toBe(
    "see support_0b7c3a52-9d1e-4c8a-8f2b-3e5d7a9c1b64 and request_7",
  )
})

test("the SDK is configured for errors only: no replay, tracing, breadcrumbs or user data", () => {
  const options = Sentry.getClient()!.getOptions()
  expect(options.tracesSampleRate).toBeUndefined()
  expect(options.maxBreadcrumbs).toBe(0)
  const names = options.integrations.map((integration) => integration.name)
  for (const name of ["Replay", "BrowserTracing", "Breadcrumbs", "HttpContext", "Feedback"])
    expect(names).not.toContain(name)
})

test("without a DSN the SDK is off", () => {
  const off = buildSentryOptions(undefined, "")
  expect([off.dsn, off.enabled, off.release]).toEqual([undefined, false, undefined])
})
