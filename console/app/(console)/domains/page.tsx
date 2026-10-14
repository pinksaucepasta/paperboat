import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { DomainList } from "@/components/domains/domain-list"

export const metadata: Metadata = { title: sections.domains.title }

export default function Page() {
  return (
    <SectionPage id="domains">
      <DomainList />
    </SectionPage>
  )
}
