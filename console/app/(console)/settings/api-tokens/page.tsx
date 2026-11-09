import type { Metadata } from "next"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { ApiTokens } from "@/components/access/api-tokens"
import { SectionPage } from "@/components/section-page"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.apiTokens.title }

// The first page of tokens, read on the server so the list is in the first HTML.
const reads = (workspace: string) => [
  {
    key: queryKeys.apiTokens(workspace),
    path: `workspaces/${encodeURIComponent(workspace)}/api-tokens?limit=50`,
    infinite: true,
  },
]

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="apiTokens">
        <ApiTokens />
      </SectionPage>
    </WithSession>
  )
}
