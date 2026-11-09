"use client"

import { parseAsString, useQueryState } from "nuqs"
import { NuqsAdapter } from "nuqs/adapters/next/app"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { MachinePicker } from "@/components/machine-picker"
import { Crumb, PageHeader } from "@/components/page-header"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Frame for a sidebar section: the page's one content column. Machine-scoped sections keep the
 * chosen machine in `?machine=`. `narrow`: a settings page of label–value rows, kept to max-w-3xl at
 * the column's left edge (DESIGN.md, Pages).
 */
export function SectionPage({
  id,
  narrow,
  children,
}: {
  id: keyof typeof sections
  narrow?: boolean
  children?: React.ReactNode
}) {
  const section = sections[id]
  return (
    <>
      <PageHeader>
        <Crumb>{section.title}</Crumb>
        {section.machineScoped && (
          <Suspense fallback={<Skeleton className="my-1.5 h-4 w-28" />}>
            {/* URL state reads search params, so it lives inside this boundary, not at the root. */}
            <NuqsAdapter>
              <MachineScope allowAll={!("oneMachine" in section && section.oneMachine)} />
            </NuqsAdapter>
          </Suspense>
        )}
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">
        {narrow ? <div className="max-w-3xl">{children}</div> : children}
      </main>
    </>
  )
}

function MachineScope({ allowAll }: { allowAll: boolean }) {
  const [machine, setMachine] = useQueryState("machine", parseAsString)
  return <MachinePicker allowAll={allowAll} value={machine} onChange={setMachine} />
}
