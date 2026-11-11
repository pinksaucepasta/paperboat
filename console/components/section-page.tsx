"use client"

import { useSearchParams } from "next/navigation"
import { parseAsString, useQueryState } from "nuqs"
import { NuqsAdapter } from "nuqs/adapters/next/app"
import { Suspense } from "react"
import { sections } from "@/lib/sections"
import { MachinePicker } from "@/components/machine-picker"
import { Crumb, CrumbLink, PageHeader } from "@/components/page-header"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Frame for a sidebar section: the page's one content column. Machine-scoped sections keep the
 * chosen machine in `?machine=` (DESIGN.md, Pages).
 */
export function SectionPage({
  id,
  children,
}: {
  id: keyof typeof sections
  children?: React.ReactNode
}) {
  const section = sections[id]
  return (
    <>
      <PageHeader>
        <Suspense fallback={<Crumb>{section.title}</Crumb>}>
          <SectionCrumb href={section.href} title={section.title} />
        </Suspense>
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
        {children}
      </main>
    </>
  )
}

/** The section's name: a link back to its list while a machine is chosen (the machine is the item), plain text on the list itself. */
function SectionCrumb({ href, title }: { href: string; title: string }) {
  const onItem = Boolean(useSearchParams().get("machine"))
  return onItem ? <CrumbLink href={href}>{title}</CrumbLink> : <Crumb>{title}</Crumb>
}

function MachineScope({ allowAll }: { allowAll: boolean }) {
  const [machine, setMachine] = useQueryState("machine", parseAsString)
  return <MachinePicker allowAll={allowAll} value={machine} onChange={setMachine} />
}
