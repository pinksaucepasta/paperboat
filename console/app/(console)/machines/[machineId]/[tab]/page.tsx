import type { Metadata } from "next"
import { notFound } from "next/navigation"
import { Suspense } from "react"
import { machineTabs } from "@/lib/sections"
import { MachineTabView } from "@/components/machine-tab-view"

export const metadata: Metadata = { title: "Machine" }

// The machine shell renders this only after the machine loads in the browser, so there is
// nothing for the build-time instant-navigation check to see.
export const instant = false

/** A machine-scoped view of a section. Sections not built yet render nothing here. */
export default function MachineTabPage({ params }: PageProps<"/machines/[machineId]/[tab]">) {
  return (
    <Suspense>
      <Tab params={params} />
    </Suspense>
  )
}

async function Tab({ params }: Pick<PageProps<"/machines/[machineId]/[tab]">, "params">) {
  const { tab } = await params
  if (!machineTabs.some((entry) => entry.slug && entry.slug === tab)) notFound()
  return <MachineTabView tab={tab} />
}
