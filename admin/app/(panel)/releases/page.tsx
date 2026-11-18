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
    <Page title="Releases">
      <Tabs value={view} onChange={setView} options={[["floor", "Minimum version"], ["authority", "Release authority"]]} />
      {!admin ? <p className="py-12 text-center text-sm text-muted-foreground">Admins only.</p> : view === "floor" ? (
        <div className="space-y-6">
          <ClientFloor />
          <ClientFloor component="edge" />
          <ClientFloor component="relay" />
        </div>
      ) : <ReleaseAuthority />}
    </Page>
  )
}
