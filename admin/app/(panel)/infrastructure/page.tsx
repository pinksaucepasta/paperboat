"use client"

import { useState } from "react"
import { Keys } from "@/components/infra/keys"
import { ManagedNodes } from "@/components/infra/managed-nodes"
import { RegionsTab } from "@/components/infra/regions"
import { ControlOperations, Health, Publications } from "@/components/infra/runtime"
import { SelfhostTab } from "@/components/infra/selfhost"
import { ServerNodesTab } from "@/components/infra/server-nodes"
import { Page } from "@/components/page"
import { Tabs } from "@/components/tabs"

type View = "selfhost" | "managed" | "regions" | "servers" | "relays" | "tunnels" | "publications" | "operations" | "keys"

export default function InfrastructurePage() {
  const [view, setView] = useState<View>("selfhost")
  return (
    <Page title="Servers">
      <Tabs
        value={view}
        onChange={setView}
        options={[
          ["selfhost", "Self-hosted"],
          ["managed", "Managed nodes"],
          ["regions", "Regions"],
          ["servers", "Server nodes"],
          ["relays", "Relay health"],
          ["tunnels", "Tunnels"],
          ["publications", "Hostnames"],
          ["operations", "Operations"],
          ["keys", "Keys"],
        ]}
      />
      {view === "selfhost" && <SelfhostTab />}
      {view === "managed" && <ManagedNodes />}
      {view === "regions" && <RegionsTab />}
      {view === "servers" && <ServerNodesTab />}
      {view === "relays" && <Health kind="relay" />}
      {view === "tunnels" && <Health kind="tunnel" />}
      {view === "publications" && <Publications />}
      {view === "operations" && <ControlOperations />}
      {view === "keys" && <Keys />}
    </Page>
  )
}
