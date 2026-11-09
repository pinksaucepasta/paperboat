import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { TunnelList } from "@/components/tunnels/tunnel-list"

export const metadata: Metadata = { title: sections.tunnels.title }

export default function Page() {
  return (
    <SectionPage id="tunnels">
      <UrlScope>
        <TunnelList />
      </UrlScope>
    </SectionPage>
  )
}
