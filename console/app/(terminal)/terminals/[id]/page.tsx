import type { Metadata } from "next"
import { Suspense } from "react"
import { TerminalPage } from "@/components/terminal/terminal-page"

export const metadata: Metadata = { title: "Terminal" }

/** A terminal gets the whole tab: no console sidebar, just its own bar and People (see TerminalPage). */
export default function Page({ params }: PageProps<"/terminals/[id]">) {
  return (
    // The id streams in after the static shell.
    <Suspense fallback={<div className="h-svh bg-background" />}>
      <Terminal params={params} />
    </Suspense>
  )
}

async function Terminal({ params }: Pick<PageProps<"/terminals/[id]">, "params">) {
  const { id } = await params
  return <TerminalPage id={id} />
}
