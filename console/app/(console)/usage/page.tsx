import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { UsagePage } from "@/components/usage/usage-page"

export const metadata: Metadata = { title: sections.usage.title }

export default function Page() {
  return (
    <SectionPage id="usage">
      {/* The page reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <UsagePage />
      </Suspense>
    </SectionPage>
  )
}
