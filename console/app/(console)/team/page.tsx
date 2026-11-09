import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { TeamPage } from "@/components/team/team-page"
import { ListSkeleton } from "@/components/skeletons"

export const metadata: Metadata = { title: sections.team.title }

export default function Page() {
  return (
    <SectionPage id="team">
      {/* TeamPage reads ?team= and ?tab= (useSearchParams), which a prerender can't know. */}
      <Suspense fallback={<ListSkeleton rows={3} rowClassName="h-16" />}>
        <TeamPage />
      </Suspense>
    </SectionPage>
  )
}
