import { Suspense } from "react"
import { MachineShell } from "@/components/machine-shell"
import { CrumbLink, PageHeader } from "@/components/page-header"
import { WithSession } from "@/components/with-session"
import { queryKeys } from "@/lib/query-keys"

export default function MachineLayout({ params, children }: LayoutProps<"/machines/[machineId]">) {
  return (
    <Suspense fallback={<MachineFallback />}>
      <Machine params={params}>{children}</Machine>
    </Suspense>
  )
}

async function Machine({ params, children }: Pick<LayoutProps<"/machines/[machineId]">, "params" | "children">) {
  const { machineId } = await params
  // Read on the server, the machine's header and tabs are in the first HTML.
  const reads = () => [{ key: queryKeys.machine(machineId), path: `machines/${encodeURIComponent(machineId)}` }]
  return (
    <WithSession reads={reads}>
      <MachineShell machineId={machineId}>{children}</MachineShell>
    </WithSession>
  )
}

function MachineFallback() {
  return (
    <PageHeader>
      <CrumbLink href="/">Machines</CrumbLink>
    </PageHeader>
  )
}
