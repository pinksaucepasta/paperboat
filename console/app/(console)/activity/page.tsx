import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { ActivityPage } from "@/components/activity/activity-page"

export const metadata: Metadata = { title: sections.activity.title }

export default function Page() {
  return (
    <SectionPage id="activity">
      {/* The page reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <ActivityPage />
      </Suspense>
    </SectionPage>
  )
}
