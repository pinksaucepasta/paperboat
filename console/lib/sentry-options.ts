import type { Options } from "@sentry/core"
import { maskEvent } from "@/lib/sentry-mask"

/**
 * One configuration for the browser, Node and edge runtimes. Errors only: no tracing, no session replay, no breadcrumbs
 * (they would carry console output, clicks and request URLs), no user, cookie, header, body or query collection; every event goes through maskEvent.
 * Without NEXT_PUBLIC_SENTRY_DSN the SDK is off and reports nothing. The release is the build's commit (next.config.ts).
 */
/** Integrations that collect request data, breadcrumbs, sessions, replay or feedback: never run here. */
const dropped =
  /^(Breadcrumbs|HttpContext|BrowserSession|Console|Http|NodeFetch|Replay|ReplayCanvas|Feedback|BrowserTracing|GraphQLClient)$/

export const buildSentryOptions = (dsn: string | undefined, release: string | undefined): Options => ({
  dsn,
  enabled: Boolean(dsn),
  release: release || undefined,
  environment: process.env.NODE_ENV,
  dataCollection: { userInfo: false, cookies: false, httpHeaders: false, httpBodies: [], urlQueryParams: false },
  sendClientReports: false,
  maxBreadcrumbs: 0,
  beforeBreadcrumb: () => null,
  beforeSend: maskEvent,
  // A cancelled request is not a failure.
  ignoreErrors: [/^AbortError/, "ResizeObserver loop completed with undelivered notifications."],
  integrations: (defaults) => defaults.filter((integration) => !dropped.test(integration.name)),
})

// Read as literals so Next inlines them into the browser bundle.
export const sentryOptions = buildSentryOptions(
  process.env.NEXT_PUBLIC_SENTRY_DSN,
  process.env.NEXT_PUBLIC_SENTRY_RELEASE,
)
