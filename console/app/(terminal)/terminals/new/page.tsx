import type { Metadata } from "next"
import { Suspense } from "react"
import { StartTerminal } from "@/components/terminal/start-terminal"

export const metadata: Metadata = { title: "New terminal" }

export default function Page() {
  return (
    // The machine comes from the URL, which streams in after the static shell.
    <Suspense fallback={<div className="h-svh bg-background" />}>
      <StartTerminal />
    </Suspense>
  )
}
