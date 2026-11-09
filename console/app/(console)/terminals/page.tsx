import type { Metadata } from "next"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { TerminalList } from "@/components/terminal/terminal-list"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.terminals.title }

// The first page of open terminals, read on the server so the rows (and whether the Running column
// shows) are in the first HTML. Terminals are yours across workspaces, so the read isn't per workspace.
const reads = () => [
  {
    key: queryKeys.terminalPage({ closed: false, q: "", page: 1 }),
    path: "terminal-sessions?limit=50&offset=0&closed=false",
    select: (data: Record<string, unknown>) => {
      const page = data as { sessions: unknown[]; pagination: { total: number } }
      return { items: page.sessions, total: page.pagination.total }
    },
  },
]

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="terminals">
        <UrlScope>
          <TerminalList />
        </UrlScope>
      </SectionPage>
    </WithSession>
  )
}
