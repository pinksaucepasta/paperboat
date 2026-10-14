import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { FileList } from "@/components/files/file-list"

export const metadata: Metadata = { title: sections.files.title }

export default function Page() {
  return (
    <SectionPage id="files">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <FileList />
      </Suspense>
    </SectionPage>
  )
}
