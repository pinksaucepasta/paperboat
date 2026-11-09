import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { UsagePage } from "@/components/usage/usage-page"

export const metadata: Metadata = { title: sections.usage.title }

export default function Page() {
  return (
    <SectionPage id="usage">
      {/* The page reads ?machine=, which streams in after the static shell. */}
      <UrlScope>
        <UsagePage />
      </UrlScope>
    </SectionPage>
  )
}
