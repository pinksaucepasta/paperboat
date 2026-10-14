import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { TunnelList } from "@/components/tunnels/tunnel-list"

export const metadata: Metadata = { title: sections.tunnels.title }

export default function Page() {
  return (
    <SectionPage id="tunnels">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <TunnelList />
      </Suspense>
    </SectionPage>
  )
}
