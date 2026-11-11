import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { RecordingList } from "@/components/recordings/recording-list"

export const metadata: Metadata = { title: sections.recordings.title }

export default function Page() {
  return (
    <SectionPage id="recordings">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <UrlScope>
        <RecordingList />
      </UrlScope>
    </SectionPage>
  )
}
