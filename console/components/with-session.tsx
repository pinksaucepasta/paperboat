import { Suspense } from "react"
import { prefetchSession } from "@/lib/server/prefetch"
import { ServerData } from "@/components/server-data"

/**
 * Renders `children` with the visitor's session (user, workspaces) already in the query cache, so the
 * HTML arrives with the user menu and workspace crumb filled in, and pages that read search params
 * render on the server instead of only after hydration. The static shell shows `children` meanwhile.
 */
export function WithSession({ children }: { children: React.ReactNode }) {
  return (
    <Suspense fallback={children}>
      <Session>{children}</Session>
    </Suspense>
  )
}

async function Session({ children }: { children: React.ReactNode }) {
  const { state, workspace } = await prefetchSession()
  return (
    <ServerData state={state} workspace={workspace}>
      {children}
    </ServerData>
  )
}
