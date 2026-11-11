"use client"

import { useSelectedLayoutSegment } from "next/navigation"
import { MachineOverview } from "@/components/machine-overview"
import { MachineActivity } from "@/components/activity/activity-page"
import { MachineConfigSync } from "@/components/config/config-sync"
import { MachineUsage } from "@/components/usage/usage-page"
import { MachineEnvironment } from "@/components/environment/env-list"
import { MachineFiles } from "@/components/files/file-list"
import { MachineTerminals } from "@/components/terminal/terminal-list"
import { MachinePreviews } from "@/components/tunnels/preview-list"
import { MachineTunnels } from "@/components/tunnels/tunnel-list"
import { MachineRecordings } from "@/components/recordings/recording-list"

const views: Record<string, () => React.ReactNode> = {
  "": MachineOverview,
  terminals: MachineTerminals,
  previews: MachinePreviews,
  tunnels: MachineTunnels,
  files: MachineFiles,
  environment: MachineEnvironment,
  config: MachineConfigSync,
  usage: MachineUsage,
  activity: MachineActivity,
  recordings: MachineRecordings,
}

/**
 * A machine tab's view. Without `tab` it takes the open tab from the route, which the prerendered
 * shell knows before the server has read the tab parameter.
 */
export function MachineTabView({ tab }: { tab?: string }) {
  const segment = useSelectedLayoutSegment()
  const View = views[tab ?? segment ?? ""]
  return View ? <View /> : null
}
