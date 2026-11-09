import type { Metadata } from "next"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { ActivityPage } from "@/components/activity/activity-page"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.activity.title }

// The first page of the unfiltered log (what the page opens on), read on the server. Its key is the
// filter's query string, which for the default filter is the workspace alone (lib/activity.ts).
const reads = (workspace: string) => {
  const params = new URLSearchParams({ workspace }).toString()
  return [{ key: queryKeys.activity(params), path: `activity?${params}&limit=50`, infinite: true }]
}

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="activity">
        {/* The page reads ?machine=, which streams in after the static shell. */}
        <UrlScope>
          <ActivityPage />
        </UrlScope>
      </SectionPage>
    </WithSession>
  )
}
