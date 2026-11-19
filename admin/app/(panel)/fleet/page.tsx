"use client"

import { useState } from "react"
import { FleetOffline, FleetRollouts, FleetVersions } from "@/components/fleet"
import { Page } from "@/components/page"
import { Tabs } from "@/components/tabs"

type View = "versions" | "rollouts" | "offline"

export default function FleetPage() {
  const [view, setView] = useState<View>("versions")
  return (
    <Page title="Fleet">
      <Tabs value={view} onChange={setView} options={[["versions", "Versions"], ["rollouts", "Rollouts"], ["offline", "Offline"]]} />
      {view === "versions" ? <FleetVersions /> : view === "rollouts" ? <FleetRollouts /> : <FleetOffline />}
    </Page>
  )
}
