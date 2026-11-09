import type { Metadata } from "next"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { TunnelList } from "@/components/tunnels/tunnel-list"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.tunnels.title }

type TunnelPage = { items: unknown[] | null; total: number }

// The first page of tunnels (or the empty state), read on the server; shaped as tunnelsPageQuery's
// cursorPage returns it.
const reads = (workspace: string) => [
  {
    key: queryKeys.tunnelsPage(workspace, { workspace, q: "", page: 1 }),
    path: `tunnels?${new URLSearchParams({ workspace, limit: "50" })}`,
    select: (data: Record<string, unknown>) => {
      const page = data as unknown as TunnelPage
      return { items: page.items ?? [], total: page.total, page: 1 }
    },
  },
]

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="tunnels">
        <UrlScope>
          <TunnelList />
        </UrlScope>
      </SectionPage>
    </WithSession>
  )
}
