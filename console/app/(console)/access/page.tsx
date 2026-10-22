import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { AccessPage } from "@/components/access/access-page"
import { SectionPage } from "@/components/section-page"

export const metadata: Metadata = { title: sections.access.title }

export default function Page() {
  return (
    <SectionPage id="access">
      <div className="mx-auto max-w-4xl">
        <AccessPage />
      </div>
    </SectionPage>
  )
}
