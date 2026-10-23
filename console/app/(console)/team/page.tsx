import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { TeamPage } from "@/components/team/team-page"
import { Skeleton } from "@/components/ui/skeleton"

export const metadata: Metadata = { title: sections.team.title }

export default function Page() {
  return (
    <SectionPage id="team">
      <div className="mx-auto max-w-3xl">
        {/* TeamPage reads ?team= and ?tab= (useSearchParams), which a prerender can't know. */}
        <Suspense fallback={<Skeleton className="h-40 w-full" />}>
          <TeamPage />
        </Suspense>
      </div>
    </SectionPage>
  )
}
