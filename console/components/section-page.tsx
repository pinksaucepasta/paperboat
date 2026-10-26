"use client"

import { parseAsString, useQueryState } from "nuqs"
import { NuqsAdapter } from "nuqs/adapters/next/app"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { MachinePicker } from "@/components/machine-picker"
import { Crumb, PageHeader } from "@/components/page-header"
import { Skeleton } from "@/components/ui/skeleton"

/** Frame for a sidebar section. Machine-scoped sections keep the chosen machine in `?machine=`. */
export function SectionPage({ id, children }: { id: keyof typeof sections; children?: React.ReactNode }) {
  const section = sections[id]
  return (
    <>
      <PageHeader>
        <Crumb>{section.title}</Crumb>
        {section.machineScoped && (
          <Suspense fallback={<Skeleton className="h-5 w-28" />}>
            {/* URL state reads search params, so it lives inside this boundary, not at the root. */}
            <NuqsAdapter>
              <MachineScope allowAll={!("oneMachine" in section && section.oneMachine)} />
            </NuqsAdapter>
          </Suspense>
        )}
      </PageHeader>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8 md:py-8">{children}</main>
    </>
  )
}

function MachineScope({ allowAll }: { allowAll: boolean }) {
  const [machine, setMachine] = useQueryState("machine", parseAsString)
  return <MachinePicker allowAll={allowAll} value={machine} onChange={setMachine} />
}
