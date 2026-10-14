import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { ConfigSync } from "@/components/config/config-sync"

export const metadata: Metadata = { title: sections.config.title }

export default function Page() {
  return (
    <SectionPage id="config">
      {/* The view reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <ConfigSync />
      </Suspense>
    </SectionPage>
  )
}
