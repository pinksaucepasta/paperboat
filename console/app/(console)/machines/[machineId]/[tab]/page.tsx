import type { Metadata } from "next"
import { notFound } from "next/navigation"
import { Suspense } from "react"
import { machineTabs } from "@/lib/sections"
import { MachineActivity } from "@/components/activity/activity-page"
import { MachineConfigSync } from "@/components/config/config-sync"
import { MachineUsage } from "@/components/usage/usage-page"
import { MachineEnvironment } from "@/components/environment/env-list"
import { MachineFiles } from "@/components/files/file-list"
import { MachineTerminals } from "@/components/terminal/terminal-list"
import { MachinePreviews } from "@/components/tunnels/preview-list"
import { MachineTunnels } from "@/components/tunnels/tunnel-list"

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
  if (tab === "terminals") return <MachineTerminals />
  if (tab === "previews") return <MachinePreviews />
  if (tab === "tunnels") return <MachineTunnels />
  if (tab === "files") return <MachineFiles />
  if (tab === "environment") return <MachineEnvironment />
  if (tab === "config") return <MachineConfigSync />
  if (tab === "usage") return <MachineUsage />
  if (tab === "activity") return <MachineActivity />
  return null
}
