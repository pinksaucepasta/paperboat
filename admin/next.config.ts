import type { NextConfig } from "next"

// The panel is reached through its own origin (ADMIN_ORIGIN), also in development.
const publicHost = process.env.ADMIN_ORIGIN ? new URL(process.env.ADMIN_ORIGIN).hostname : undefined

const csp = [
  "default-src 'self'",
  // Next injects inline bootstrap scripts and styles; nothing else may run, load or frame.
  "script-src 'self' 'unsafe-inline'" + (process.env.NODE_ENV === "production" ? "" : " 'unsafe-eval'"),
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data:",
  "font-src 'self' data:",
  "connect-src 'self'",
  "frame-ancestors 'none'",
  "base-uri 'none'",
  "form-action 'self'",
].join("; ")

const nextConfig: NextConfig = {
  // tools/admin-live.sh builds into the slot the live server is not running from.
  distDir: process.env.NEXT_DIST_DIR || ".next",
  allowedDevOrigins: publicHost ? [publicHost] : [],
  devIndicators: false,
  poweredByHeader: false,
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "Content-Security-Policy", value: csp },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "no-referrer" },
          { key: "X-Frame-Options", value: "DENY" },
          // Operator data is never cached by the browser or a proxy.
          { key: "Cache-Control", value: "no-store" },
        ],
      },
    ]
  },
  experimental: {
    turbopackFileSystemCacheForDev: false,
  },
}

export default nextConfig
