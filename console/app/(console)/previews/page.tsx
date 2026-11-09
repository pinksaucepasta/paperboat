import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { PreviewList } from "@/components/tunnels/preview-list"

export const metadata: Metadata = { title: sections.previews.title }

export default function Page() {
  return (
    <SectionPage id="previews">
      <UrlScope>
        <PreviewList />
      </UrlScope>
    </SectionPage>
  )
}
