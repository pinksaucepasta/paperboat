import type { NextConfig } from "next"

// The console is reached through its public origin (WORKOS_REDIRECT_URI), also in development.
const publicHost = process.env.WORKOS_REDIRECT_URI ? new URL(process.env.WORKOS_REDIRECT_URI).hostname : undefined

const nextConfig: NextConfig = {
  cacheComponents: true,
  partialPrefetching: true,
  allowedDevOrigins: publicHost ? [publicHost] : [],
  // The Funnel serves the dev server for review; the badge covers real UI. Errors still overlay.
  devIndicators: false,
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

export default nextConfig
