"use client"

import { useState } from "react"
import { ClientFloor, ReleaseAuthority } from "@/components/releases"
import { Page } from "@/components/page"
import { Tabs } from "@/components/tabs"
import { useOperator } from "@/lib/api"

type View = "floor" | "authority"

export default function ReleasesPage() {
  const [view, setView] = useState<View>("floor")
  const admin = useOperator()?.role === "admin"
  return (
    <Page title="Releases" description="The oldest CLI version we still serve, and the signed release decisions that control what updaters install.">
      <Tabs value={view} onChange={setView} options={[["floor", "Minimum CLI version"], ["authority", "Release authority"]]} />
      {!admin ? <p className="text-sm text-muted-foreground">Only administrators can read or change release policy.</p> : view === "floor" ? <ClientFloor /> : <ReleaseAuthority />}
    </Page>
  )
}
