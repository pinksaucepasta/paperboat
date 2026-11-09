import type { Metadata } from "next"
import { cookies } from "next/headers"
import { redirect } from "next/navigation"
import { Suspense } from "react"
import { controlPlaneFetch } from "@/lib/server/control-plane"
import { TerminalJoin } from "@/components/terminal/terminal-join"

export const metadata: Metadata = { title: "Join a terminal" }

/**
 * The one link a terminal is shared by. It needs no session (proxy.ts lets /terminal/ through):
 * people with access go on to the terminal, everyone else signs in or joins with a code.
 */
export default function Page({ params, searchParams }: PageProps<"/terminal/[id]">) {
  return (
    <Suspense fallback={<div className="h-svh bg-background" />}>
      <Join params={params} searchParams={searchParams} />
    </Suspense>
  )
}

async function Join({ params, searchParams }: Pick<PageProps<"/terminal/[id]">, "params" | "searchParams">) {
  const [{ id }, { join }] = await Promise.all([params, searchParams])
  // Someone with access through their account goes straight to the terminal, without a spinner hop.
  if (await hasAccountAccess(id)) redirect(`/terminals/${encodeURIComponent(id)}`)
  return <TerminalJoin id={id} withCode={join === "code"} />
}

async function hasAccountAccess(id: string): Promise<boolean> {
  try {
    const response = await controlPlaneFetch(`/v1/terminal-sessions/${encodeURIComponent(id)}/browser-session`, {
      headers: { accept: "application/json", cookie: (await cookies()).toString() },
      signal: AbortSignal.timeout(3_000),
    })
    if (!response.ok) return false
    const body = (await response.json()) as { data?: { access_method?: string } }
    return body.data?.access_method === "account"
  } catch {
    // The page asks again in the browser.
    return false
  }
}
