"use client"

import { errorReference } from "@/lib/error-reference"

/** The root layout itself crashed: no theme or components are available, so the page is plain. */
export default function GlobalError({ error, retry }: { error: Error & { digest?: string }; retry: () => void }) {
  const reference = errorReference(error)
  return (
    <html lang="en">
      <body
        style={{
          fontFamily: "system-ui, sans-serif",
          display: "grid",
          placeItems: "center",
          minHeight: "100vh",
          margin: 0,
        }}
      >
        <div style={{ textAlign: "center", maxWidth: 420, padding: 16 }}>
          <h1 style={{ fontSize: 18, fontWeight: 500 }}>Something went wrong</h1>
          <p style={{ fontSize: 14, color: "#666" }}>
            Paperboat couldn’t load. Try again; if it keeps happening, send us the reference below.
          </p>
          <button onClick={() => retry()} style={{ padding: "6px 14px", marginBottom: 12 }}>
            Try again
          </button>
          <p style={{ fontSize: 12, color: "#666" }}>
            Reference: <code>{reference}</code>
          </p>
        </div>
      </body>
    </html>
  )
}
