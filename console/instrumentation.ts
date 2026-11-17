import * as Sentry from "@sentry/nextjs"

export async function register() {
  if (process.env.NEXT_RUNTIME === "nodejs") await import("./sentry.server.config")
  if (process.env.NEXT_RUNTIME === "edge") await import("./sentry.edge.config")
}

/** Errors Next.js catches while rendering on the server or running a route handler. */
export const onRequestError = Sentry.captureRequestError
