import type { Metadata } from "next"
import { MachineOverview } from "@/components/machine-overview"

export const metadata: Metadata = { title: "Machine" }

// The machine shell renders this only after the machine loads in the browser, so there is
// nothing for the build-time instant-navigation check to see.
export const instant = false

export default function MachinePage() {
  return <MachineOverview />
}
