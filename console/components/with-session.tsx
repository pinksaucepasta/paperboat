import { headers } from "next/headers"
import { redirect } from "next/navigation"
import { Suspense } from "react"
import { safeReturnPath } from "@/lib/server/control-plane"
import { prefetchSession, type PageRead } from "@/lib/server/prefetch"
import { ServerData } from "@/components/server-data"

/**
 * Renders `children` with the visitor's session (user, workspaces, billing) and the page's own first
 * `reads` already in the query cache, so the HTML arrives filled in, and pages that read search
 * params render on the server instead of only after hydration. The static shell shows `children`
 * meanwhile.
 */
export function WithSession({
  reads,
  children,
}: {
  reads?: (workspace: string) => PageRead[]
  children: React.ReactNode
}) {
  return (
    <Suspense fallback={children}>
      <Session reads={reads}>{children}</Session>
    </Suspense>
  )
}

async function Session({ reads, children }: { reads?: (workspace: string) => PageRead[]; children: React.ReactNode }) {
  const { state, workspace, viewer, ended } = await prefetchSession(reads)
  // An expired cookie still passes the proxy; send it to sign-in now, not after the page loads.
  if (ended) {
    const path = safeReturnPath((await headers()).get("x-pb-path"))
    redirect(path && path !== "/" ? `/auth/sign-in?return_to=${encodeURIComponent(path)}` : "/auth/sign-in")
  }
  return (
    <ServerData state={state} workspace={workspace} viewer={viewer}>
      {children}
    </ServerData>
  )
}
