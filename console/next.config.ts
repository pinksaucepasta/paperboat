import { execSync } from "node:child_process"
import type { NextConfig } from "next"
import { withSentryConfig } from "@sentry/nextjs/config"

// The console is reached through its public origin (WORKOS_REDIRECT_URI), also in development.
const publicHost = process.env.WORKOS_REDIRECT_URI ? new URL(process.env.WORKOS_REDIRECT_URI).hostname : undefined

// Error reports name the build by its commit (SENTRY_RELEASE overrides it); a build outside a checkout has none.
const release =
  process.env.SENTRY_RELEASE ||
  (() => {
    try {
      return execSync("git rev-parse HEAD", { stdio: ["ignore", "pipe", "ignore"] }).toString().trim()
    } catch {
      return undefined
    }
  })()

const nextConfig: NextConfig = {
  env: { NEXT_PUBLIC_SENTRY_RELEASE: release ?? "" },
  // tools/console-live.sh builds into the slot the live server is not running from.
  distDir: process.env.NEXT_DIST_DIR || ".next",
  cacheComponents: true,
  partialPrefetching: true,
  allowedDevOrigins: publicHost ? [publicHost] : [],
  // The Funnel serves the dev server for review; the badge covers real UI. Errors still overlay.
  devIndicators: false,
  // The preview/tunnel edge sends signed-out browsers to <console>/.paperboat/access/login (F7-03).
  async rewrites() {
    return [{ source: "/.paperboat/access/login", destination: "/access-login" }]
  },
  // Domains live on the Network page and Recordings on the Terminals page; old links still land there.
  async redirects() {
    return [
      { source: "/domains", destination: "/network#domains", permanent: false },
      { source: "/recordings", destination: "/terminals?view=recordings", permanent: false },
    ]
  },
  async headers() {
    // The Ghostty core's URL carries its version, so browsers may keep it for good.
    return [
      {
        source: "/terminal/ghostty-vt-0.5.1.wasm",
        headers: [{ key: "Cache-Control", value: "public, max-age=31536000, immutable" }],
      },
    ]
  },
  experimental: {
    // Set by `next upgrade --agent=security` (16.4.0 → 16.4.2, security advisory).
    agentUpgrade: "security",
    // The persisted dev cache served stale CSS and kept re-emitting the client entry,
    // which made every open tab reload in a loop. Dev starts are fast enough without it.
    turbopackFileSystemCacheForDev: false,
  },
}

// Source maps go to Sentry only when SENTRY_AUTH_TOKEN is set at build time; without it the build is unchanged.
// SENTRY_URL names the region's host (EU), SENTRY_ORG and SENTRY_PROJECT default to this product's.
const token = process.env.SENTRY_AUTH_TOKEN
export default withSentryConfig(nextConfig, {
  org: process.env.SENTRY_ORG ?? "pprbt",
  project: process.env.SENTRY_PROJECT ?? "console",
  authToken: token,
  sentryUrl: process.env.SENTRY_URL,
  release: { name: release },
  sourcemaps: { disable: !token },
  widenClientFileUpload: true,
  telemetry: false,
  silent: true,
})
