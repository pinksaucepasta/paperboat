import type { Metadata } from "next"
import { sections } from "@/lib/sections"
import { UrlScope } from "@/components/list-toolbar"
import { SectionPage } from "@/components/section-page"
import { EnvironmentList } from "@/components/environment/env-list"
import { EnvNamesSwitch } from "@/components/environment/env-names-switch"

export const metadata: Metadata = { title: sections.environment.title }

export default function Page() {
  return (
    <SectionPage id="environment">
      <UrlScope>
        <EnvironmentList />
        <div className="mt-6">
          <EnvNamesSwitch />
        </div>
      </UrlScope>
    </SectionPage>
  )
}
