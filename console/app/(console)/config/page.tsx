import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { ConfigSync } from "@/components/config/config-sync"

export const metadata: Metadata = { title: sections.config.title }

export default function Page() {
  return (
    <SectionPage id="config">
      {/* The view reads ?machine=, which streams in after the static shell. */}
      <UrlScope>
        <ConfigSync />
      </UrlScope>
    </SectionPage>
  )
}
