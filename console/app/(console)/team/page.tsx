import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { TeamPage } from "@/components/team/team-page"

export const metadata: Metadata = { title: sections.team.title }

export default function Page() {
  return (
    <SectionPage id="team">
      <div className="mx-auto max-w-3xl">
        <TeamPage />
      </div>
    </SectionPage>
  )
}
