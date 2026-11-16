import type { Metadata } from "next"
import { queryKeys } from "@/lib/query-keys"
import { sections } from "@/lib/sections"
import { Recipes } from "@/components/recipes/recipes"
import { SectionPage } from "@/components/section-page"
import { WithSession } from "@/components/with-session"

export const metadata: Metadata = { title: sections.recipes.title }

// The list is read on the server, so it is in the first HTML.
const reads = (workspace: string) => [
  { key: queryKeys.recipes(workspace), path: `workspaces/${encodeURIComponent(workspace)}/recipes`, field: "recipes" },
]

export default function Page() {
  return (
    <WithSession reads={reads}>
      <SectionPage id="recipes">
        <Recipes />
      </SectionPage>
    </WithSession>
  )
}
