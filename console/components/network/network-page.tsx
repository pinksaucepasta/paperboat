"use client"

import { useWorkspace } from "@/lib/api"
import { DomainList } from "@/components/domains/domain-list"
import { EdgesSection } from "@/components/network/edges-section"
import { PortNames } from "@/components/network/port-names"
import { RegionsSection } from "@/components/network/regions-section"
import { RelaysSection } from "@/components/network/relays-section"
import { scopeOf } from "@/lib/network"

/*
 * Paperboat's regions and your own regions, and which regions your traffic uses. Users see regions, never
 * Paperboat's nodes; your own machines keep their names and addresses, grouped under the region you named
 * (machines of one region back each other up). Machines connect directly whenever they can; a relay carries
 * what can't go directly (it never sees inside the encryption); an edge is where your tunnels and previews meet
 * the internet (browser HTTPS ends there).
 */
export function NetworkPage() {
  const workspace = useWorkspace()

  return (
    <div className="flex flex-col gap-8">
      <RelaysSection scope={scopeOf(workspace)} />
      <EdgesSection
        workspace={workspace?.id ?? ""}
        manage={workspace?.kind === "personal" || workspace?.role === "owner" || workspace?.role === "admin"}
      />

      <DomainList />

      <PortNames />

      <RegionsSection workspace={workspace} />
    </div>
  )
}
