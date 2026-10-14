import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { PreviewList } from "@/components/tunnels/preview-list"

export const metadata: Metadata = { title: sections.previews.title }

export default function Page() {
  return (
    <SectionPage id="previews">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <PreviewList />
      </Suspense>
    </SectionPage>
  )
}
