import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { ActivityPage } from "@/components/activity/activity-page"

export const metadata: Metadata = { title: sections.activity.title }

export default function Page() {
  return (
    <SectionPage id="activity">
      {/* The page reads ?machine=, which streams in after the static shell. */}
      <UrlScope>
        <ActivityPage />
      </UrlScope>
    </SectionPage>
  )
}
