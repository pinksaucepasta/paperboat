import type { Metadata } from "next"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { SectionPage } from "@/components/section-page"
import { EnvironmentList } from "@/components/environment/env-list"
import { EnvNamesSwitch } from "@/components/environment/env-names-switch"

export const metadata: Metadata = { title: sections.environment.title }

export default function Page() {
  return (
    <SectionPage id="environment">
      {/* The list reads ?machine=, which streams in after the static shell. */}
      <Suspense>
        <EnvironmentList />
        <div className="mt-6">
          <EnvNamesSwitch />
        </div>
      </Suspense>
    </SectionPage>
  )
}
