import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { NetworkPage } from "@/components/network/network-page"

export const metadata: Metadata = { title: sections.network.title }

export default function Page() {
  return (
    <SectionPage id="network">
      <NetworkPage />
    </SectionPage>
  )
}
