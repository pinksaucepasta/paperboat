import type { Metadata } from "next"
import { Suspense } from "react"
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
  return <TerminalJoin id={id} withCode={join === "code"} />
}
