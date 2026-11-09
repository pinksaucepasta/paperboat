import type { Metadata } from "next"
import { Suspense } from "react"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { FileList } from "@/components/files/file-list"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.files.title }

// Requests and transfers sit above the Inbox; read on the server, they don't push it down on load.
const reads = (workspace: string) => [
  {
    key: queryKeys.inboxRequests(workspace),
    path: `team-inbox/requests?${new URLSearchParams({ workspace })}`,
    field: "requests",
  },
]

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="files">
        {/* The list reads ?machine=, which streams in after the static shell. */}
        <Suspense>
          <FileList />
        </Suspense>
      </SectionPage>
    </WithSession>
  )
}
