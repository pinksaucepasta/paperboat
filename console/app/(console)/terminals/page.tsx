import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { TerminalList } from "@/components/terminal/terminal-list"

export const metadata: Metadata = { title: sections.terminals.title }

export default function Page() {
  return (
    <SectionPage id="terminals">
      <UrlScope>
        <TerminalList />
      </UrlScope>
    </SectionPage>
  )
}
