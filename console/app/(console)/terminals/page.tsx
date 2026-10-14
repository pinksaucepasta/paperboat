import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { TerminalList } from "@/components/terminal/terminal-list"

export const metadata: Metadata = { title: sections.terminals.title }

export default function Page() {
  return (
    <SectionPage id="terminals">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <TerminalList />
      </Suspense>
    </SectionPage>
  )
}
