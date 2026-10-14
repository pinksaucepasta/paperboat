import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { EnvironmentList } from "@/components/environment/env-list"

export const metadata: Metadata = { title: sections.environment.title }

export default function Page() {
  return (
    <SectionPage id="environment">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <EnvironmentList />
      </Suspense>
    </SectionPage>
  )
}
