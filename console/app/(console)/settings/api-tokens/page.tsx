import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { ApiTokens } from "@/components/access/api-tokens"
import { SectionPage } from "@/components/section-page"

export const metadata: Metadata = { title: sections.apiTokens.title }

export default function Page() {
  return (
    <SectionPage id="apiTokens">
      <div className="mx-auto max-w-3xl">
        <ApiTokens />
      </div>
    </SectionPage>
  )
}
