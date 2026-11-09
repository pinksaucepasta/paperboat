import type { Metadata } from "next"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { ConfigSync } from "@/components/config/config-sync"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.config.title }

type StatusPage = { environments: unknown[] | null; pagination: { total: number } }

// The GitHub connection and the first page of every machine's sync, read on the server so the page
// arrives filled in; shaped as lib/config-sync.ts's configStatusQuery and cursorPage return them.
const reads = () => [
  { key: queryKeys.githubStatus, path: "github/status" },
  {
    // The same options the page's list asks with (All machines: the ones that sync).
    key: queryKeys.configStatus({ q: "", state: "configured", page: 1 }),
    path: "config-sync/status?limit=50&state=configured",
    select: (data: Record<string, unknown>) => {
      const page = data as unknown as StatusPage
      return { items: page.environments ?? [], total: page.pagination.total, page: 1 }
    },
  },
]

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="config">
        {/* The view reads ?machine=, which streams in after the static shell. */}
        <UrlScope>
          <ConfigSync />
        </UrlScope>
      </SectionPage>
    </WithSession>
  )
}
